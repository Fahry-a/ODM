package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"time"

	"odm/internal/storage"
)

// resume.go holds the resume/checkpoint/integrity half of Task: control-file
// persistence (persistControl), the sampled server-side compare, per-chunk
// SHA-256 bookkeeping and their verification on --continue. Split from
// task.go purely for reviewability — no behavior change; these methods are
// the same Task receivers and share its locks/fields.

// verifyResumedChunks samples a handful of the chunks the control file claims
// are already on disk and compares them against the server with ranged GETs. A
// mismatch means the file changed server-side (with no ETag to detect it) or
// the local copy is stale/corrupt, so the caller re-downloads from scratch.
// Single-stream downloads are skipped: the single whole-file chunk can't be
// sampled without effectively re-downloading it, and its resume is guarded by
// the ETag/size checks. Fails safe — any request/read error is treated as a
// mismatch.
//
// resumeSampleSpans returns up to n completed chunk spans for the sampled
// server-side compare, with ABSOLUTE offsets and each region's own chunk size.
// For the both profile this samples region1 AND region2 — previously only
// t.queue (region1) was sampled, so server drift in the second region went
// undetected. For the single-queue profiles the layout chunk size is used so
// the sampled span matches the actual chunk (an aria2c segment, not the 4 MiB
// odm default).
func (t *Task) resumeSampleSpans(n int) []Chunk {
	pr := t.probe.Load()
	if t.engines == nil {
		cs := t.layoutChunkSize(0)
		if cs < 1 {
			cs = t.opts.ChunkSize
		}
		return t.queue.CompletedSpans(cs, pr.TotalSize, n)
	}
	var out []Chunk
	// region1: odm fixed chunks of opts.ChunkSize over [0, splitAt).
	out = append(out, t.engines[0].CompletedSpans(t.opts.ChunkSize, t.splitAt, n)...)
	// region2: aria2c segments over [splitAt, total). Translate to absolute.
	seg := t.region2ChunkSize()
	if seg < 1 {
		seg = t.opts.ChunkSize
	}
	for _, s := range t.engines[1].CompletedSpans(seg, pr.TotalSize-t.splitAt, n) {
		out = append(out, Chunk{Start: t.splitAt + s.Start, End: t.splitAt + s.End})
	}
	return out
}

func (t *Task) verifyResumedChunks(ctx context.Context) error {
	pr := t.probe.Load()
	if pr == nil || pr.SingleStream || pr.TotalSize <= 0 {
		return nil
	}
	spans := t.resumeSampleSpans(resumeVerifyChunks)
	for _, s := range spans {
		end := s.Start + resumeProbeLen - 1
		if end > s.End {
			end = s.End
		}
		want := int(end - s.Start + 1)
		chkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		// Sample through the client that actually fetched the span: region2
		// (both profile) was downloaded over the h2 client, so comparing it
		// via the h1 client would flag a valid resume as a mismatch on servers
		// that serve ranges differently per protocol.
		cli := t.client
		if t.engines != nil && s.Start >= t.splitAt {
			cli = t.engines[1].Client()
		}
		resp, err := cli.GetRange(chkCtx, pr.FinalURL, s.Start, end, "")
		if err != nil {
			cancel()
			return fmt.Errorf("resume check at %d: %w", s.Start, err)
		}
		got := make([]byte, want)
		_, rerr := io.ReadFull(resp.Body, got)
		resp.Body.Close()
		cancel()
		if rerr != nil {
			return fmt.Errorf("resume check at %d: %v", s.Start, rerr)
		}
		disk := make([]byte, want)
		if _, derr := t.disk.ReadAt(disk, s.Start); derr != nil {
			return fmt.Errorf("resume check at %d: %v", s.Start, derr)
		}
		if !bytes.Equal(got, disk) {
			return fmt.Errorf("resume check at %d: on-disk data differs from server", s.Start)
		}
	}
	return nil
}

// storeChunkHash records the SHA-256 hex digest of a successfully completed
// chunk, keyed by its Start offset. Guarded by t.mu — the same lock
// persistControl holds when reading the map, so no second mutex is needed.
// This runs once per completed chunk, never on the copy hot loop.
func (t *Task) storeChunkHash(start int64, sum string) {
	t.mu.Lock()
	t.chunkHashes[start] = sum
	t.mu.Unlock()
}

// clearChunkHashes drops all recorded hashes — used when a resume integrity
// check fails and the download restarts from scratch, so stale hashes can't
// leak into the fresh run's checkpoints.
func (t *Task) clearChunkHashes() {
	t.mu.Lock()
	t.chunkHashes = make(map[int64]string)
	t.mu.Unlock()
}

// restoreChunkHashes seeds the in-memory hash map from a control file being
// resumed, so hashes of previously-completed chunks survive into subsequent
// checkpoints instead of being dropped (which would downgrade the next resume
// to the legacy server-compare fallback). Only hashes for offsets the queues
// actually accepted as completed are carried over.
//
// The control file keys hashes by ABSOLUTE offset (persistControl translates
// region2's base), so the both profile must translate each queue's relative
// completed offsets to absolute before the lookup — otherwise region2's hashes
// are silently dropped and the next checkpoint persists them gone.
func (t *Task) restoreChunkHashes(cf *storage.ControlFile) {
	if len(cf.ChunkHashes) == 0 || t.queue == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.engines == nil {
		for _, off := range t.queue.CompletedOffsets() {
			if sum, ok := cf.ChunkHashes[off]; ok {
				t.chunkHashes[off] = sum
			}
		}
		return
	}
	for _, off := range t.engines[0].CompletedOffsets() {
		if sum, ok := cf.ChunkHashes[off]; ok {
			t.chunkHashes[off] = sum
		}
	}
	for _, off := range t.engines[1].CompletedOffsets() {
		abs := off + t.engines[1].base
		if sum, ok := cf.ChunkHashes[abs]; ok {
			t.chunkHashes[abs] = sum
		}
	}
}

// snapshotChunkHashes returns the recorded hashes for the given completed
// offsets — the subset of t.chunkHashes that has actually been marked done.
// Called from persistControl with t.mu held.
func (t *Task) snapshotChunkHashes(completed []int64) map[int64]string {
	if len(t.chunkHashes) == 0 {
		return nil
	}
	out := make(map[int64]string, len(completed))
	for _, off := range completed {
		if sum, ok := t.chunkHashes[off]; ok {
			out[off] = sum
		}
	}
	return out
}

// verifyResumedData checks the integrity of completed chunks before a resume
// trusts them. It composes TWO independent guarantees:
//
//   - Local disk integrity: when every completed chunk has a recorded per-chunk
//     SHA-256 hash (control files written by this version), each completed
//     chunk's on-disk bytes are hashed and compared — catching local
//     corruption/truncation with no network traffic. Partial coverage (e.g. a
//     hash-less legacy control file that was resumed once: new chunks get
//     hashes, legacy completed chunks don't) does NOT fail the resume; it
//     simply downgrades to the legacy server-side compare.
//
//   - Server drift: the sampled server-side compare (verifyResumedChunks)
//     detects that the server replaced the file with same-size content since
//     the original download. The stored hashes are of the ORIGINAL bytes, so
//     they cannot catch such a replacement by themselves — this is why the
//     two checks are complementary, never alternatives. It no-ops for
//     single-stream/sizeless downloads.
//
// Either failure → the caller re-downloads from scratch.
func (t *Task) verifyResumedData(ctx context.Context, cf *storage.ControlFile) error {
	// Local disk integrity, per-chunk hashes (only when coverage is complete).
	if t.hasFullHashCoverage(cf) {
		if err := t.verifyResumedHashes(cf); err != nil {
			return err
		}
	}
	// Server drift, sampled compare — independent of hashes.
	return t.verifyResumedChunks(ctx)
}

// hasFullHashCoverage reports whether every completed chunk in cf has a
// recorded per-chunk hash — the precondition for hash-based local
// verification. Partial coverage must not fail a resume: it just falls back
// to the legacy server-side compare for the whole set.
func (t *Task) hasFullHashCoverage(cf *storage.ControlFile) bool {
	if cf == nil || len(cf.ChunkHashes) == 0 {
		return false
	}
	for off := range cf.CompletedOffsets() {
		if _, ok := cf.ChunkHash(off); !ok {
			return false
		}
	}
	return true
}

// verifyResumedHashes verifies every completed chunk recorded in cf against
// the on-disk bytes, using the per-chunk SHA-256 hashes captured while the
// chunk was first downloaded and persisted in the control file. This catches
// LOCAL corruption of already-written chunks — flipped/zero-filled bytes,
// truncation, a partial write. Server-side drift is covered separately by
// verifyResumedChunks (the stored hashes are of the original bytes, so they
// cannot detect a same-size replacement). A single mismatch fails the whole
// resume so the caller re-downloads from scratch.
func (t *Task) verifyResumedHashes(cf *storage.ControlFile) error {
	pr := t.probe.Load()
	if pr == nil || pr.TotalSize <= 0 {
		return nil
	}
	offsets := cf.CompletedOffsets()
	starts := make([]int64, 0, len(offsets))
	for off := range offsets {
		starts = append(starts, off)
	}
	// Deterministic order: error diagnostics should name a stable chunk, not
	// whichever map iteration happened to visit first.
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	buf := make([]byte, 64*1024)
	for _, off := range starts {
		want, ok := cf.ChunkHash(off)
		if !ok {
			return fmt.Errorf("resume hash check at %d: no recorded hash for completed chunk", off)
		}
		start, end := t.chunkSpan(off)
		if end < start {
			return fmt.Errorf("resume hash check at %d: invalid span [%d, %d]", off, start, end)
		}
		h := sha256.New()
		for pos := start; pos <= end; {
			n := int64(len(buf))
			if pos+n-1 > end {
				n = end - pos + 1
			}
			if _, err := t.disk.ReadAt(buf[:n], pos); err != nil {
				return fmt.Errorf("resume hash check at %d: read: %w", off, err)
			}
			_, _ = h.Write(buf[:n])
			pos += n
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("resume hash check at %d: on-disk data differs from recorded hash", off)
		}
	}
	return nil
}

// chunkSpan returns the byte span [start, end] that a chunk beginning at
// `start` occupies in the current layout, mirroring the queue split
// arithmetic. The single-stream whole-file chunk (Start=0, End=-1) spans the
// entire known size. The span size must match the LAYOUT's chunk size — the
// aria2c profile splits into large segments, and the both profile's region2
// uses those same segment sizes — not opts.ChunkSize, or hashing the wrong
// range would fail every resume verify (see layoutChunkSize).
func (t *Task) chunkSpan(start int64) (int64, int64) {
	pr := t.probe.Load()
	if pr.SingleStream {
		return 0, pr.TotalSize - 1
	}
	cs := t.layoutChunkSize(start)
	end := start + cs - 1
	if pr.TotalSize > 0 && end >= pr.TotalSize {
		end = pr.TotalSize - 1
	}
	return start, end
}

// layoutChunkSize returns the size of the chunk/segment beginning at `start`
// in the CURRENT layout:
//   - odm profile: opts.ChunkSize (fixed work-stealing chunks)
//   - aria2c profile: the static segment size (AriaSplit), which is usually
//     much larger than opts.ChunkSize
//   - both profile: opts.ChunkSize in region1, the aria2c segment size in
//     region2 (offsets >= splitAt)
//
// Resume hash verification hashes the exact span a completed chunk occupies;
// using opts.ChunkSize for an aria2c/both-region2 segment hashes only a
// prefix of the bytes the recorded SHA-256 covers, so the digest never matches
// and every resume is discarded as a failed integrity check.
func (t *Task) layoutChunkSize(start int64) int64 {
	if t.engines != nil && start >= t.splitAt {
		if seg := t.region2ChunkSize(); seg > 0 {
			return seg
		}
	}
	if t.opts.Profile == "aria2c" {
		if seg := t.effectiveChunkSize(); seg > 0 {
			return seg
		}
	}
	// Mirror NewChunkQueue's silent default: a non-positive ChunkSize would
	// otherwise produce end = start-1 and spuriously fail every hash verify.
	cs := t.opts.ChunkSize
	if cs < 1 {
		cs = defaultChunkSize
	}
	return cs
}
