// SPDX-License-Identifier: MIT
//
// engine.go holds the split arithmetic for the aria2c profile (and the
// aria2-style region of the both profile). The odm profile uses ChunkQueue's
// fixed-chunk work-stealing layout; aria2c divides the file into a SMALL
// number of equal segments — one per connection — matching aria2c's
// --split/-s and --min-split-size/-k semantics:
//
//	split_eff = min(split, size / (2*minSplit))
//	segment   = size / split_eff   (rounded up)
//
// A static split has NO work-stealing: each connection takes exactly one
// segment and finishes (aria2c's actual behaviour). A failed segment is
// retried by the same worker, never requeued to another.

package download

import (
	"slices"
	"sync"

	"odm/internal/transport"
)

// workQueue is the common surface the worker loop needs from a chunk source.
// ChunkQueue (odm, work-stealing fixed chunks) and StaticQueue (aria2c, one
// segment per worker) implement it, so the worker stays profile-agnostic.
type workQueue interface {
	Next() (Chunk, bool)
	Requeue(c Chunk, maxWorkerAttempts int) bool
	MarkDone(c Chunk)
	CompletedOffsets() []int64
	CompletedSpans(chunkSize, totalSize int64, n int) []Chunk
	ResetCompletedOffsets(done map[int64]struct{}, totalSize int64) (int64, bool)
}

// Engine couples a workQueue with the transport client and byte-region base
// it belongs to. A both-profile task has two engines — region1 [0, splitAt)
// on the h1 client, region2 [splitAt, end) on the h2 client — sharing the
// same file at disjoint offsets. A single-profile task is one engine with
// base 0.
type Engine struct {
	q      workQueue
	client *transport.Client
	base   int64 // absolute file offset this engine's chunks map to
}

// Next delegates to the queue.
func (e *Engine) Next() (Chunk, bool) { return e.q.Next() }

// Requeue delegates to the queue.
func (e *Engine) Requeue(c Chunk, max int) bool { return e.q.Requeue(c, max) }

// MarkDone delegates to the queue.
func (e *Engine) MarkDone(c Chunk) { e.q.MarkDone(c) }

// CompletedOffsets delegates to the queue.
func (e *Engine) CompletedOffsets() []int64 { return e.q.CompletedOffsets() }

// CompletedSpans delegates to the queue.
func (e *Engine) CompletedSpans(chunkSize, totalSize int64, n int) []Chunk {
	return e.q.CompletedSpans(chunkSize, totalSize, n)
}

// ResetCompletedOffsets delegates to the queue.
func (e *Engine) ResetCompletedOffsets(done map[int64]struct{}, totalSize int64) (int64, bool) {
	return e.q.ResetCompletedOffsets(done, totalSize)
}

// Client returns the engine's transport client (h1 for region 1, h2 for
// region 2 in the both profile).
func (e *Engine) Client() *transport.Client { return e.client }

// AbsStart maps a queue-internal chunk start to its absolute file offset.
func (e *Engine) AbsStart(rel int64) int64 { return e.base + rel }

// AriaSplit computes the effective split count and segment size for a file of
// totalSize given the user's --split (N) and --min-split-size (minSplit)
// preferences. It mirrors aria2c's rule that a byte range is only split when
// it is at least 2*minSplit big — a 20 MiB file with minSplit 15M yields
// split_eff=1 (single segment), and with minSplit 10M yields 2.
//
// Returns (n, chunkSize) with n≥1 and chunkSize≥1 for any size>0. For a
// sizeless or non-positive totalSize it returns (1, totalSize) — the caller
// degrades to the odm single-stream path anyway.
func AriaSplit(totalSize, split, minSplit int64) (n int64, chunkSize int64) {
	if totalSize <= 0 || split < 1 {
		return 1, totalSize
	}
	if minSplit < 1 {
		minSplit = 1
	}
	n = split
	if maxN := totalSize / (2 * minSplit); maxN < n && maxN >= 1 {
		n = maxN
	}
	return n, (totalSize + n - 1) / n
}

// StaticQueue is a fixed-list work queue with no work-stealing: exactly n
// segments covering [0, totalSize) contiguously (last one possibly short),
// each to be taken by exactly one worker. Next returns each segment once;
// Requeue is a no-op (aria2c model — a failed segment is retried by the same
// worker via the per-chunk retry loop in downloadChunk).
//
// The segment layout is deterministic from (totalSize, n), so resume can
// rebuild the same boundaries from the control file without storing the
// segment map.
type StaticQueue struct {
	mu     sync.Mutex
	chunks []Chunk // segments; each claimed at most once
	next   int
	done   map[int64]struct{}
}

// NewStaticQueue builds a static split of totalSize into n equal segments.
func NewStaticQueue(totalSize, n int64) *StaticQueue {
	if n < 1 {
		n = 1
	}
	q := &StaticQueue{done: map[int64]struct{}{}}
	if totalSize <= 0 {
		return q
	}
	seg := (totalSize + n - 1) / n
	for start := int64(0); start < totalSize; {
		end := start + seg - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		q.chunks = append(q.chunks, Chunk{Index: len(q.chunks), Start: start, End: end})
		start = end + 1
	}
	return q
}

// Next returns the next unclaimed segment, or ok=false when all have been
// claimed (including segments pre-seeded as done by ResetCompletedOffsets).
func (q *StaticQueue) Next() (Chunk, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.next < len(q.chunks) {
		c := q.chunks[q.next]
		q.next++
		if _, ok := q.done[c.Start]; !ok {
			return c, true
		}
	}
	return Chunk{}, false
}

// Requeue is a no-op: the aria2c model retries a failed segment inside the
// same worker (downloadChunk's per-attempt loop), never handing it to
// another worker. Always returns true so the worker loop's retry path never
// treats a segment as permanently failed here.
func (q *StaticQueue) Requeue(_ Chunk, _ int) bool { return true }

// MarkDone records a completed segment. Idempotent.
func (q *StaticQueue) MarkDone(c Chunk) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.done[c.Start] = struct{}{}
}

// CompletedOffsets returns the sorted offsets of completed segments.
func (q *StaticQueue) CompletedOffsets() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, 0, len(q.done))
	for off := range q.done {
		out = append(out, off)
	}
	slices.Sort(out)
	return out
}

// CompletedSpans returns up to n completed segment spans (ascending) for
// resume-time sampling.
func (q *StaticQueue) CompletedSpans(chunkSize, totalSize int64, n int) []Chunk {
	q.mu.Lock()
	defer q.mu.Unlock()
	starts := make([]int64, 0, len(q.done))
	for off := range q.done {
		starts = append(starts, off)
	}
	slices.Sort(starts)
	if n <= 0 || len(starts) <= n {
		n = len(starts)
	}
	out := make([]Chunk, 0, n)
	for i := 0; i < n && i < len(starts); i++ {
		off := starts[i]
		end := off + chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		out = append(out, Chunk{Start: off, End: end})
	}
	return out
}

// ResetCompletedOffsets pre-seeds completed segments (resume). Segments in
// `done` are skipped by Next (advancing past them). Returns the bytes already
// complete and whether the set is compatible with the layout (always true for
// static splits — offsets are absolute and segments are large).
//
// totalSize is accepted for interface uniformity with ChunkQueue (which uses
// it to compute chunk bytes); a static segment's byte count is fully
// determined by the segment map, so it is intentionally unused here.
func (q *StaticQueue) ResetCompletedOffsets(done map[int64]struct{}, totalSize int64) (int64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var already int64
	for _, c := range q.chunks {
		if _, ok := done[c.Start]; ok {
			q.done[c.Start] = struct{}{}
			already += c.End - c.Start + 1
		}
	}
	// Skip already-done segments when handing work out, so resume doesn't
	// re-download them. q.next advances past every completed offset.
	q.next = 0
	for q.next < len(q.chunks) {
		if _, ok := q.done[q.chunks[q.next].Start]; ok {
			q.next++
			continue
		}
		break
	}
	return already, true
}
