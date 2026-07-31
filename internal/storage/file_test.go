package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestOpenForWrite_Preallocates verifies that a known size pre-allocates the
// file to exactly that length so chunk writes can land at any offset without
// the file growing out from under them (§11.1).
func TestOpenForWrite_Preallocates(t *testing.T) {
	dir := t.TempDir()
	const size = 1 << 16
	f, err := OpenForWrite(dir, "blob.bin", size)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if got := f.Size(); got != size {
		t.Fatalf("Size = %d, want %d", got, size)
	}
	info, err := os.Stat(f.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != size {
		t.Fatalf("on-disk size = %d, want pre-allocated %d", info.Size(), size)
	}
	if pr := filepath.Clean(f.Path()); filepath.Dir(pr) != dir {
		t.Fatalf("Path unexpected dir: %q", pr)
	}
}

// TestOpenForWrite_SizelessStream: size<0 must NOT pre-allocate — the file
// grows as bytes are written (sizeless/streaming download fallback, §11.2).
func TestOpenForWrite_SizelessStream(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenForWrite(dir, "stream.dat", -1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if f.Size() != -1 {
		t.Fatalf("Size = %d, want -1 (sizeless)", f.Size())
	}
	info, err := os.Stat(f.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("sizeless file should start at 0 bytes, got %d", info.Size())
	}
	if _, err := f.WriteAt([]byte("hello"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err = os.Stat(f.Path())
	if err != nil {
		t.Fatalf("stat after write: %v", err)
	}
	if info.Size() != 5 {
		t.Fatalf("sizeless file should grow to 5, got %d", info.Size())
	}
}

// TestOpenForWrite_CreatesMissingDir ensures the destination directory is
// created (recursively) when absent — saves callers an explicit mkdir.
func TestOpenForWrite_CreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	f, err := OpenForWrite(nested, "x.bin", 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("nested dir not created: %v", err)
	}
}

// TestOpenForWrite_RejectsEmptyName guards the obvious bad-input case.
func TestOpenForWrite_RejectsEmptyName(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenForWrite(dir, "", 1); err == nil {
		t.Fatalf("want error for empty name")
	}
}

// TestFile_WriteAtReadAt往返 ensures what's written at an offset can be read
// back via ReadAt — the round-trip checksum verification depends on this.
func TestFile_WriteAtReadAt_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenForWrite(dir, "rw.bin", 1<<14)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	payload := bytes.Repeat([]byte{'Z'}, 4096)
	if _, err := f.WriteAt(payload, 1<<13); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := f.ReadAt(got, 1<<13); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read-back mismatch")
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// TestFile_ConcurrentWriteAt_NoOverlap stresses the §11.1 guarantee: many
// workers writing non-overlapping chunks must produce exactly the union of
// their bytes with no torn writes. The chunk-queue engine relies on this
// being safe without a mutex.
func TestFile_ConcurrentWriteAt_NoOverlap(t *testing.T) {
	dir := t.TempDir()
	const chunk = 256
	const chunks = 32
	f, err := OpenForWrite(dir, "concurrent.bin", int64(chunk*chunks))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	var wg sync.WaitGroup
	for i := 0; i < chunks; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(i * chunk)
			payload := bytes.Repeat([]byte{byte('A' + i%26)}, chunk)
			if _, err := f.WriteAt(payload, off); err != nil {
				t.Errorf("worker %d write: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Verify every byte lands in the right slot — a torn write would shift
	// the expected pattern.
	for i := 0; i < chunks; i++ {
		buf := make([]byte, chunk)
		if _, err := f.ReadAt(buf, int64(i*chunk)); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read chunk %d: %v", i, err)
		}
		want := byte('A' + i%26)
		for j, b := range buf {
			if b != want {
				t.Fatalf("chunk %d byte %d = %#x, want %#x (torn write?)", i, j, b, want)
			}
		}
	}

	// Full-file assertion: the concatenation of all disjoint spans must be
	// exactly what was written, byte-identical.
	got, err := io.ReadAll(mustReader(t, f.Path()))
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(got) != chunk*chunks {
		t.Fatalf("final size = %d, want %d", len(got), chunk*chunks)
	}
	for i := 0; i < chunks; i++ {
		want := byte('A' + i%26)
		for j := 0; j < chunk; j++ {
			if got[i*chunk+j] != want {
				t.Fatalf("full-file byte %d = %#x, want %#x", i*chunk+j, got[i*chunk+j], want)
			}
		}
	}
}

// mustReader opens path for reading or fails the test.
func mustReader(t *testing.T, path string) io.Reader {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestFile_Reader re-opens the destination for sequential reads (the
// checksum verifier uses this).
func TestFile_Reader(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenForWrite(dir, "rdr.bin", -1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.WriteAt([]byte("checksum me"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := f.Reader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	if string(got) != "checksum me" {
		t.Fatalf("got %q", got)
	}
}

// TestFile_CloseIdempotent: calling Close twice must not error (the engine
// may call Close defensively).
func TestFile_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenForWrite(dir, "tmp.bin", 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
