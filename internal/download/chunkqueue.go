package download

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
)

// Chunk is one slice of work for a worker: download [Start, End] inclusive and
// write it to the destination at offset Start. Index is the chunk's ordinal
// (used for control-file bookkeeping & resume). Tail-split pieces share the
// Index of the root chunk they belong to.
type Chunk struct {
	Index int
	Start int64
	End   int64 // inclusive; End<0 ⇒ to EOF/unknown length
}

// ChunkQueue is the shared, per-task work list. Workers pull the next
// available chunk as soon as they finish their previous one, instead of each
// owning a fixed byte-range — this is what avoids the straggler problem of
// static equal-split segmentation: a slow worker simply processes fewer chunks.
//
// When the queue is drained but at least one piece is still downloading, an
// idle caller carves the larger half of the remaining range out of the
// straggler (tail-splitting): the owner stops at the boundary and the
// remainder re-enters the queue for an idle worker. The control file only
// ever lists *root* chunk offsets — sub-ranges are tracked as coverage until
// the whole root is hashed, so resume verification is unchanged.
//
// It also tracks completed chunk offsets so a resume pass can skip them
// efficiently, and the per-chunk worker-failure counter so a failed chunk can
// be requeued (another worker retries it) up to a bounded number of passes.
type ChunkQueue struct {
	mu        sync.Mutex
	chunks    []Chunk            // remaining work
	completed map[int64]struct{} // root-aligned offsets only
	failed    map[int]int        // worker-level failure count per chunk Index

	chunkSize int64
	totalSize int64

	// active holds the in-flight pieces keyed by piece start (several pieces
	// of the same root chunk may coexist). Tail-splitting chooses the victim
	// from this set and a piece's owner advances to the boundary it requests.
	// chunkSize <= 0 disables tail-splitting entirely (sizeless stream).
	active map[int64]*activePiece
	// coverage tracks completed sub-ranges of a root chunk until the root is
	// complete and joins `completed`.
	coverage map[int64][]byteSpan
	// wake is closed (and replaced) on every queue mutation; Next's idle wait
	// reads it under the lock, so no wake-up is lost.
	wake chan struct{}
}

// activePiece is the live record for one in-flight piece (a whole chunk or a
// piece carved out by tail-splitting).
type activePiece struct {
	Index   int
	Start   int64
	End     int64 // inclusive
	written atomic.Int64
	// stopAfter, when >0, is the byte position INTO THIS PIECE at which the
	// owner must stop writing.
	stopAfter atomic.Int64
}

type byteSpan struct {
	start, end int64 // inclusive
}

// minTailSplit is the smallest remaining range worth carving out of a
// straggler; anything smaller would not pay for another HTTP request.
const minTailSplit = 64 << 10

// errChunkSplit is NOT an error surface for the task: copyChunkFrom returns it
// when a requested tail-split boundary was reached, which means "stop at the
// boundary and hand the unwritten remainder back to the queue".
var errChunkSplit = errors.New("tail split point reached")

// splitPointError carries the piece-local byte count written up to the stop
// boundary.
type splitPointError struct {
	written int64
}

func (e *splitPointError) Error() string { return "chunk reached the tail-split boundary" }

// defaultChunkSize is used when the caller specifies no chunk size (or a
// non-positive one). Defined once because both the queue splitter and the
// resume hash verifier (chunkSpan) must agree on the effective chunk
// boundaries — a mismatch would compute the wrong span for a completed chunk.
const defaultChunkSize = 4 * 1024 * 1024

// Work-stealing queue depth: a fixed chunk size starves the connection budget
// on files smaller than conns×chunkSize (a 2 MiB file is ONE 4 MiB chunk, so
// -c 4 runs one connection). The layout therefore aims for
// queueChunksPerConn chunks per connection, keeping the user's --chunk-size as
// the upper bound and minQueueChunk as the floor so tiny files don't turn into
// a request storm.
const (
	minQueueChunk      = 128 << 10
	queueChunksPerConn = 2
)

// tunedChunkSize returns the chunk size to build the work-stealing queue with:
// `configured` (--chunk-size, default 4 MiB) capped so the file yields about
// queueChunksPerConn chunks per connection, floored at minQueueChunk. A
// non-positive totalSize (sizeless stream), an empty file or a single
// connection keeps the configured size.
func tunedChunkSize(totalSize int64, conns int, configured int64) int64 {
	if configured <= 0 {
		configured = defaultChunkSize
	}
	if totalSize <= 0 || conns < 2 {
		return configured
	}
	want := totalSize / (int64(conns) * queueChunksPerConn)
	if want >= configured {
		return configured
	}
	if want < minQueueChunk {
		want = minQueueChunk
	}
	// The configured size is always the upper bound: an explicit --chunk-size
	// below minQueueChunk (tests, deliberate tiny chunks) is honored, not
	// overridden by the floor.
	if want > configured {
		want = configured
	}
	return want
}

// NewChunkQueue builds the chunk list for a file of totalSize using chunkSize
// bytes per chunk. If totalSize<0 (sizeless stream) the queue has a
// single "whole file" chunk (Start=0 End=-1, Index=0) — the single-stream
// degeneration.
func NewChunkQueue(totalSize, chunkSize int64) *ChunkQueue {
	q := &ChunkQueue{
		completed: map[int64]struct{}{},
		failed:    map[int]int{},
		chunkSize: chunkSize,
		totalSize: totalSize,
		active:    map[int64]*activePiece{},
		coverage:  map[int64][]byteSpan{},
		wake:      make(chan struct{}),
	}
	if totalSize < 0 {
		q.chunks = []Chunk{{Index: 0, Start: 0, End: -1}}
		return q
	}
	if chunkSize < 1 {
		chunkSize = defaultChunkSize
		q.chunkSize = chunkSize
	}
	if totalSize == 0 {
		// Empty file: nothing to download. Zero chunks.
		return q
	}
	var idx int
	var start int64
	for start < totalSize {
		end := start + chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		q.chunks = append(q.chunks, Chunk{Index: idx, Start: start, End: end})
		start = end + 1
		idx++
	}
	return q
}

// ResetCompletedOffsets pre-seeds the completed set with byte-offsets already
// on disk (resume). Those chunks are removed from the work list so workers
// never reprocess them. Returns the total bytes already done and whether the
// completed set is compatible with the current chunk layout. Incompatible means
// the control file lists more completed chunks than this queue holds — e.g. a
// ranged resume hitting a now single-stream URL, or a chunk-size change that
// slipped past the caller's size check — and the caller should re-download from
// scratch rather than trust stale offsets.
func (q *ChunkQueue) ResetCompletedOffsets(done map[int64]struct{}, totalSize int64) (int64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed = make(map[int]int, len(done))
	q.coverage = make(map[int64][]byteSpan)
	q.active = make(map[int64]*activePiece)
	if len(done) > len(q.chunks) {
		return 0, false
	}
	q.completed = make(map[int64]struct{}, len(done))
	kept := q.chunks[:0]
	var already int64
	for _, c := range q.chunks {
		if _, ok := done[c.Start]; ok {
			q.completed[c.Start] = struct{}{}
			already += chunkBytes(c, totalSize)
			continue
		}
		kept = append(kept, c)
	}
	q.chunks = kept
	return already, true
}

// Requeue returns a failed chunk/piece to the work list so another worker can
// retry it, and reports whether retrying is still worthwhile. Each call bumps
// the chunk's worker-level failure counter; once it exceeds maxWorkerAttempts
// the chunk is NOT requeued and Requeue returns false — the caller should treat
// the chunk as permanently failed. The chunk is pushed to the back of the queue
// so other chunks keep making progress while it cools off.
//
// If an idle worker already carved this piece's tail off (an active tail-split
// request is pending), only the unwritten prefix is re-enqueued: the remainder
// was handed to another worker and downloading it again here would duplicate
// work (and double-count progress).
func (q *ChunkQueue) Requeue(c Chunk, maxWorkerAttempts int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if ap, ok := q.active[c.Start]; ok {
		if stop := ap.stopAfter.Load(); stop > 0 {
			c.End = c.Start + stop - 1
		}
		delete(q.active, c.Start)
	}
	q.failed[c.Index]++
	if q.failed[c.Index] > maxWorkerAttempts {
		q.wakeLocked()
		return false
	}
	q.chunks = append(q.chunks, c)
	q.wakeLocked()
	return true
}

// Next pulls the next chunk to work on, or returns ok=false when the queue is
// drained and no tail-split is left to request. While at least one piece is in
// flight, an idle caller carves a half of the remaining range out of the
// straggler; the owner then Enqueue's the remainder and the waiter picks it up.
// Concurrent pulls are serialized by the mutex; since each pull is a
// pointer-assignment + slice pop this is cheap and not on the hot data path.
func (q *ChunkQueue) Next() (Chunk, bool) {
	// maxSplitAttempts caps mid-decision aborts (owner advanced past the
	// carved boundary) so an aggressively feeding straggler can't livelock
	// the idle waiter; on exhaustion the waiter parks on the wake channel
	// like any other unsplittable situation.
	const maxSplitAttempts = 8
	splitAttempts := 0
	for {
		q.mu.Lock()
		if n := len(q.chunks); n > 0 {
			c := q.chunks[0]
			q.chunks = q.chunks[1:]
			q.active[c.Start] = &activePiece{Index: c.Index, Start: c.Start, End: c.End}
			q.mu.Unlock()
			return c, true
		}
		if q.chunkSize <= 0 || len(q.active) == 0 {
			// No work left (and no tail-splitting to request).
			q.mu.Unlock()
			return Chunk{}, false
		}
		victim := q.splittableLocked()
		if victim != nil && splitAttempts < maxSplitAttempts {
			// Carve the back half of the straggler into a fresh piece that an
			// idle worker can take immediately: the owner stops at the boundary
			// when it notices while the remainder streams in parallel.
			written := victim.written.Load()
			remaining := victim.End - victim.Start + 1 - written
			split := written + remaining/2
			absSplit := victim.Start + split
			if victim.stopAfter.CompareAndSwap(0, split) {
				if victim.written.Load() > split {
					// The owner advanced past the boundary mid-decision, so the
					// remainder would re-download bytes already on disk. Back
					// the split out instead of double-counting progress.
					victim.stopAfter.Store(0)
					splitAttempts++
				} else {
					q.chunks = append(q.chunks, Chunk{Index: victim.Index, Start: absSplit, End: victim.End})
					q.wakeLocked()
				}
			}
			q.mu.Unlock()
			continue // re-check for work: the remainder may be ours
		}
		ch := q.wake
		q.mu.Unlock()
		<-ch
	}
}

// splittableLocked returns the in-flight piece with the most bytes remaining
// that may still be carved up (no stop pending, large enough to be worth
// another request), or nil when every active piece is ineligible.
func (q *ChunkQueue) splittableLocked() *activePiece {
	var best *activePiece
	var bestRem int64
	for _, a := range q.active {
		if a.stopAfter.Load() != 0 {
			continue
		}
		rem := a.End - a.Start + 1 - a.written.Load()
		if rem < minTailSplit {
			continue
		}
		if rem > bestRem {
			best, bestRem = a, rem
		}
	}
	return best
}

func (q *ChunkQueue) wakeLocked() {
	close(q.wake)
	q.wake = make(chan struct{})
}

// Release drops a piece's active record. Idempotent; used to signal waiters
// when the in-flight set changed without producing a remainder.
func (q *ChunkQueue) Release(c Chunk) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.active[c.Start]; !ok {
		return
	}
	delete(q.active, c.Start)
	q.wakeLocked()
}

// track returns the live record for the piece beginning at start, or nil.
func (q *ChunkQueue) track(start int64) *activePiece {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.active[start]
}

// splitDone records that the owner stopped its piece at a tail-split boundary.
// The written sub-range joins its root chunk's coverage. The unwritten
// remainder was already enqueued (by the worker that requested the split), so
// it is NOT re-enqueued here. Returns the root span (relative offsets) and
// whether this call completed it.
func (q *ChunkQueue) splitDone(c Chunk, written int64) (rootStart, rootEnd int64, completed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.active, c.Start)
	fullEnd := c.Start + written - 1
	if written > 0 {
		_, _, _ = q.completeLocked(c.Start, fullEnd)
	}
	q.wakeLocked()
	return q.rootOf(c.Start)
}

// completePiece is not used by the worker; MarkDone covers the full piece
// case, and splitDone covers the tail-split boundary case.
// completeLocked adds a span to its root chunk's coverage and marks the root
// completed when covered. Returns the root span and whether this call
// completed it.
func (q *ChunkQueue) completeLocked(start, end int64) (rootStart, rootEnd int64, completed bool) {
	if q.chunkSize <= 0 || end < start {
		if end >= start {
			if _, done := q.completed[start]; done {
				return start, end, true
			}
			q.completed[start] = struct{}{}
		}
		return start, end, true
	}
	idx := start / q.chunkSize
	rs := idx * q.chunkSize
	re := rs + q.chunkSize - 1
	if q.totalSize > 0 && re >= q.totalSize {
		re = q.totalSize - 1
	}
	// Idempotence: the root is already done (its bytes are on disk and the
	// offset is in `completed`); re-marking a piece must not reopen coverage.
	if _, done := q.completed[rs]; done {
		return rs, re, true
	}
	spans := q.coverage[rs]
	spans = append(spans, byteSpan{start: start, end: end})
	merged := mergeIntervals(spans)
	q.coverage[rs] = merged
	if covers(merged, rs, re) {
		delete(q.coverage, rs)
		q.completed[rs] = struct{}{}
		return rs, re, true
	}
	return rs, re, false
}

func (q *ChunkQueue) rootOf(start int64) (rootStart, rootEnd int64, completed bool) {
	idx := int64(0)
	if q.chunkSize > 0 {
		idx = start / q.chunkSize
	}
	rs := idx * q.chunkSize
	re := rs + q.chunkSize - 1
	if q.totalSize > 0 && re >= q.totalSize {
		re = q.totalSize - 1
	}
	_, done := q.completed[rs]
	return rs, re, done
}

func mergeIntervals(in []byteSpan) []byteSpan {
	sort.Slice(in, func(i, j int) bool { return in[i].start < in[j].start })
	out := make([]byteSpan, 0, len(in))
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1].end < s.start-1 {
			out = append(out, s)
			continue
		}
		if s.end > out[len(out)-1].end {
			out[len(out)-1].end = s.end
		}
	}
	return out
}

func covers(merged []byteSpan, start, end int64) bool {
	for _, s := range merged {
		if s.start <= start && end <= s.end {
			return true
		}
	}
	return false
}

// wholeChunk reports whether the piece covers the layout chunk it belongs to
// exactly (always true for the static split). A whole-chunk piece's incremental
// hash in the copy loop is exactly the root chunk's resume hash, so worker does
// not need to re-read the file from disk.
func (q *ChunkQueue) wholeChunk(c Chunk) bool {
	if q.chunkSize <= 0 { // single-stream whole-file chunk
		return true
	}
	if c.Start%q.chunkSize != 0 {
		return false
	}
	return c.End-c.Start+1 == q.chunkSize || c.End == q.totalSize-1
}

// MarkDone records that a piece finished. Split pieces join their root chunk's
// coverage; the root joins the completed set only when fully covered. Returns
// the root span and whether this call completed it.
func (q *ChunkQueue) MarkDone(c Chunk) (rootStart, rootEnd int64, completed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.active, c.Start)
	q.wakeLocked() // release idle waiters: the in-flight set shrank
	return q.completeLocked(c.Start, c.End)
}

// CompletedOffsets returns the sorted offsets of completed chunks. Split
// pieces are NOT reported — only root-aligned offsets ever appear, which is
// what the control file and resume layout require.
func (q *ChunkQueue) CompletedOffsets() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, 0, len(q.completed))
	for k := range q.completed {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CompletedSpans returns up to n completed chunk spans (ascending by start
// offset) for resume-time integrity sampling. With many completed chunks the
// sample is spread across the file — the first half of the budget covers the
// earliest chunks, the second half the latest — so a server-side change
// anywhere in the file is caught with at most n small ranged GETs.
func (q *ChunkQueue) CompletedSpans(chunkSize, totalSize int64, n int) []Chunk {
	q.mu.Lock()
	defer q.mu.Unlock()
	starts := make([]int64, 0, len(q.completed))
	for off := range q.completed {
		starts = append(starts, off)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	if n <= 0 || len(starts) <= n {
		n = len(starts)
	}
	sample := make([]int64, 0, n)
	first := (n + 1) / 2
	sample = append(sample, starts[:first]...)
	if last := n - first; last > 0 {
		sample = append(sample, starts[len(starts)-last:]...)
	}
	out := make([]Chunk, 0, len(sample))
	for _, off := range sample {
		end := off + chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		out = append(out, Chunk{Start: off, End: end})
	}
	return out
}

// chunkBytes returns how many bytes chunk c covers given totalSize. For the
// sizeless single-chunk (End<0) we can't know — return 0 so progress reports
// stay honest (sizeless tasks report progress in a coarse way elsewhere).
func chunkBytes(c Chunk, totalSize int64) int64 {
	if c.End < 0 || totalSize < 0 {
		if totalSize > 0 {
			return totalSize // only chunk in a sizeless file but we did learn size mid-download
		}
		return 0
	}
	return c.End - c.Start + 1
}
