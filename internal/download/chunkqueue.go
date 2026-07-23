package download

import (
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

// ChunkQueue is the shared, per-task work list (§11.1). Workers pull the next
// available chunk as soon as they finish their previous one, instead of each
// owning a fixed byte-range — this is what avoids the straggler problem of
// static equal-split segmentation: a slow worker simply processes fewer chunks.
//
// It also tracks completed chunk offsets so a resume pass can skip them
// efficiently. All access is mutex-guarded; the channel-based "next chunk"
// model is intentionally simple and fair (FIFO pull).
type ChunkQueue struct {
	mu        sync.Mutex
	chunks    []Chunk // remaining work
	completed map[int64]struct{}
	total     int64 // sum of completed chunk sizes
	taskDone  bool
}

// NewChunkQueue builds the chunk list for a file of totalSize using chunkSize
// bytes per chunk (§11.1). If totalSize<0 (sizeless stream) the queue has a
// single "whole file" chunk (Start=0 End=-1, Index=0) — the single-stream
// degeneration (§11.2).
func NewChunkQueue(totalSize, chunkSize int64) *ChunkQueue {
	q := &ChunkQueue{completed: map[int64]struct{}{}}
	if totalSize < 0 {
		q.chunks = []Chunk{{Index: 0, Start: 0, End: -1}}
		return q
	}
	if chunkSize < 1 {
		chunkSize = 4 * 1024 * 1024
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
// never reprocess them. Returns the total bytes already done.
func (q *ChunkQueue) ResetCompletedOffsets(done map[int64]struct{}, chunkSize, totalSize int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
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
	q.total = already
	return already
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

// MarkDone records that a chunk finished; returns the new total bytes done.
// Idempotent: re-marking an already-completed offset is a no-op (workers may
// retry after a transient error and the original write might still have landed).
func (q *ChunkQueue) MarkDone(c Chunk, totalSize int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.completed[c.Start]; !ok {
		q.completed[c.Start] = struct{}{}
		q.total += chunkBytes(c, totalSize)
	}
	return q.total
}

// Done reports whether all chunks are completed.
func (q *ChunkQueue) Done() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.chunks) == 0 && len(q.completed) > 0
}

// Remaining returns the count of un-started chunks (for the parseInto UI).
func (q *ChunkQueue) Remaining() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.chunks)
}

// CompletedCount returns total completed chunk count.
func (q *ChunkQueue) CompletedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.completed)
}

// BytesDone returns the total completed bytes.
func (q *ChunkQueue) BytesDone() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
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
