package download

import (
	"testing"
)

// TestNewChunkQueue_BoundaryInvariant asserts that NewChunkQueue produces a
// chunk list that is strictly contiguous, non-overlapping, and covers exactly
// [0, totalSize). This is the invariant storage.File.WriteAt relies on for
// lock-free concurrent writes — a gap or overlap would silently corrupt the
// output file, so it is pinned here across several total sizes.
func TestNewChunkQueue_BoundaryInvariant(t *testing.T) {
	const chunkSize = 4096
	cases := []struct {
		name      string
		totalSize int64
	}{
		{"exact multiple", 4 * chunkSize},
		{"size not divisible by chunk size", 4*chunkSize + 123},
		{"size smaller than one chunk", 100},
		{"size smaller than one chunk, non-zero", chunkSize - 1},
		{"one byte", 1},
		{"large odd remainder", 10*chunkSize + 4095},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewChunkQueue(tc.totalSize, chunkSize)
			if len(q.chunks) == 0 {
				t.Fatalf("expected >= 1 chunk for totalSize %d", tc.totalSize)
			}
			// First chunk starts at 0; last ends at totalSize-1; interior
			// chunks are strictly contiguous with no gaps or overlaps.
			if q.chunks[0].Start != 0 {
				t.Fatalf("first chunk Start = %d, want 0", q.chunks[0].Start)
			}
			last := q.chunks[len(q.chunks)-1]
			if last.End != tc.totalSize-1 {
				t.Fatalf("last chunk End = %d, want %d (totalSize-1)", last.End, tc.totalSize-1)
			}
			for i, c := range q.chunks {
				if c.Start < 0 {
					t.Fatalf("chunk %d Start = %d < 0", i, c.Start)
				}
				if c.End < c.Start-1 {
					t.Fatalf("chunk %d End = %d < Start-1 (%d)", i, c.End, c.Start)
				}
				if c.End-c.Start+1 > chunkSize {
					t.Fatalf("chunk %d spans %d bytes, larger than chunkSize %d", i, c.End-c.Start+1, chunkSize)
				}
				if i > 0 {
					prev := q.chunks[i-1]
					if c.Start != prev.End+1 {
						t.Fatalf("chunk %d Start = %d, want prev.End+1 = %d (gap or overlap)", i, c.Start, prev.End+1)
					}
				}
			}
			// Coverage: sum of chunk sizes must equal totalSize.
			var sum int64
			for _, c := range q.chunks {
				sum += c.End - c.Start + 1
			}
			if sum != tc.totalSize {
				t.Fatalf("chunks cover %d bytes, want %d", sum, tc.totalSize)
			}
			// Indexes must be 0..n-1 in order.
			for i, c := range q.chunks {
				if c.Index != i {
					t.Fatalf("chunk %d has Index %d", i, c.Index)
				}
			}
		})
	}
}

// TestNewChunkQueue_EdgeCases pins the special layouts: sizeless streams get a
// single whole-file chunk {0,-1}, and empty files get an empty queue.
func TestNewChunkQueue_EdgeCases(t *testing.T) {
	q := NewChunkQueue(-1, 4096)
	if len(q.chunks) != 1 {
		t.Fatalf("sizeless: want 1 chunk, got %d", len(q.chunks))
	}
	if q.chunks[0].Start != 0 || q.chunks[0].End != -1 || q.chunks[0].Index != 0 {
		t.Fatalf("sizeless chunk = %+v, want {Index:0 Start:0 End:-1}", q.chunks[0])
	}

	q = NewChunkQueue(0, 4096)
	if len(q.chunks) != 0 {
		t.Fatalf("empty file: want 0 chunks, got %d", len(q.chunks))
	}
}
