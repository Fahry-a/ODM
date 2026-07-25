// Package storage owns the on-disk side of a download task: pre-allocation +
// concurrent WriteAt (§11.1) and the `.odm` control file used for resume (§11.3).
//
// Chunk writes never overlap (chunk boundaries are disjoint), so concurrent
// os.File.WriteAt calls are safe without a mutex — the OS positions the write
// at the given offset. We do hold a single *os.File per task and rely on the
// kernel for offset isolation; we do NOT keep a *os.File per chunk.
package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// File wraps a destination *os.File for concurrent chunk writes + pre-allocation.
type File struct {
	path string
	f    *os.File
	size int64 // expected total; -1 if unknown (sizeless stream)
}

// OpenForWrite opens (creating as needed) the destination file at dir/name. If
// size is known (>0), the file is pre-allocated to size so chunk writes can
// land at any offset without the file growing mid-write (§11.1). size<0 keeps
// it as a plain grow-as-you-write file for sizeless streams.
func OpenForWrite(dir, name string, size int64) (*File, error) {
	if name == "" {
		return nil, errors.New("no output filename")
	}
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if size > 0 {
		// Use ftruncate to reserve the size; sparse on filesystems that
		// support it (no actual space used until written).
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("truncate %s to %d: %w", path, size, err)
		}
	}
	return &File{path: path, f: f, size: size}, nil
}

// WriteAt writes b at the given offset. Safe for concurrent use across workers
// writing distinct offsets (non-overlapping chunks).
func (w *File) WriteAt(b []byte, off int64) (int, error) {
	n, err := w.f.WriteAt(b, off)
	return n, err
}

// ReadAt is used by checksum verify after the download completes.
func (w *File) ReadAt(p []byte, off int64) (int, error) {
	return w.f.ReadAt(p, off)
}

// Size returns the configured total size (-1 if unknown).
func (w *File) Size() int64 { return w.size }

// Path returns the destination path.
func (w *File) Path() string { return w.path }

// Sync flushes pending writes to disk (called once when a task completes).
func (w *File) Sync() error { return w.f.Sync() }

// Close closes the underlying file. Idempotent: a second Close is a no-op,
// so the engine can call it defensively on both a defer and an explicit
// shutdown path without tripping os.File's "file already closed" error.
func (w *File) Close() error {
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	return f.Close()
}

// Reader returns a sequential reader over the on-disk file (for checksum).
func (w *File) Reader() (io.ReadCloser, error) {
	return os.Open(w.path)
}
