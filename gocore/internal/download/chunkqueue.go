package download

import (
	"sort"
	"sync"
)

// Chunk is one slice of work for a worker: download [Start, End] inclusive and
// write it to the destination at offset Start. Index is the chunk's ordinal
// (used for control-file bookkeeping & resume).
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
// It also tracks completed chunk offsets so a resume pass can skip them
// efficiently, and the per-chunk worker-failure counter so a failed chunk can
// be requeued (another worker retries it) up to a bounded number of passes.
// All access is mutex-guarded; the channel-based "next chunk" model is
// intentionally simple and fair (FIFO pull).
type ChunkQueue struct {
	mu        sync.Mutex
	chunks    []Chunk // remaining work
	completed map[int64]struct{}
	failed    map[int]int // worker-level failure count per chunk Index
}

// defaultChunkSize is used when the caller specifies no chunk size (or a
// non-positive one). Defined once because both the queue splitter and the
// resume hash verifier (chunkSpan) must agree on the effective chunk
// boundaries — a mismatch would compute the wrong span for a completed chunk.
const defaultChunkSize = 4 * 1024 * 1024

// NewChunkQueue builds the chunk list for a file of totalSize using chunkSize
// bytes per chunk. If totalSize<0 (sizeless stream) the queue has a
// single "whole file" chunk (Start=0 End=-1, Index=0) — the single-stream
// degeneration.
func NewChunkQueue(totalSize, chunkSize int64) *ChunkQueue {
	q := &ChunkQueue{completed: map[int64]struct{}{}, failed: map[int]int{}}
	if totalSize < 0 {
		q.chunks = []Chunk{{Index: 0, Start: 0, End: -1}}
		return q
	}
	if chunkSize < 1 {
		chunkSize = defaultChunkSize
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

// Requeue returns a failed chunk to the work list so another worker can retry
// it, and reports whether retrying is still worthwhile. Each call bumps the
// chunk's worker-level failure counter; once it exceeds maxWorkerAttempts the
// chunk is NOT requeued and Requeue returns false — the caller should treat the
// chunk as permanently failed. The chunk is pushed to the back of the queue so
// other chunks keep making progress while it cools off.
func (q *ChunkQueue) Requeue(c Chunk, maxWorkerAttempts int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[c.Index]++
	if q.failed[c.Index] > maxWorkerAttempts {
		return false
	}
	q.chunks = append(q.chunks, c)
	return true
}

// Next pulls the next chunk to work on, or returns ok=false when the queue is
// drained. Concurrent pulls are serialized by the mutex; since each pull is a
// pointer-assignment + slice pop this is cheap and not on the hot data path.
func (q *ChunkQueue) Next() (Chunk, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.chunks) == 0 {
		return Chunk{}, false
	}
	c := q.chunks[0]
	q.chunks = q.chunks[1:]
	return c, true
}

// MarkDone records that a chunk finished. Idempotent: re-marking an
// already-completed offset is a no-op (workers may retry after a transient
// error and the original write might still have landed).
func (q *ChunkQueue) MarkDone(c Chunk) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.completed[c.Start]; !ok {
		q.completed[c.Start] = struct{}{}
	}
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
