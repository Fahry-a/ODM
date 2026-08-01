package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ControlFile is the JSON payload of `<filename>.odm` (PRD §11.3). It records
// enough state to resume: the source URL (to revalidate the server hasn't
// shipped a different file via ETag/Content-Length drift — kept but not
// enforced strictly in MVP), the total size, chunk size, and the set of chunk
// indices already written. On a resume we re-queue only the missing chunks.
//
// Extended fields (v0.2.0+) provide richer diagnostics and validation:
//   - CreatedAt / UpdatedAt: timestamps for age-of-control-file checks
//   - Connections: how many parallel connections were used (informational)
//   - UserAgent: the UA string sent to the server (consistency on resume)
//   - ODMVersion: which ODM version created this file (compatibility)
//   - Checksum: file hash if known from --checksum flag (integrity on resume)
//
// ChunkHashes records the lowercase hex SHA-256 of every completed chunk's
// bytes, keyed by the chunk's Start byte offset. A resume verifies these
// against local disk to catch on-disk corruption of already-written chunks;
// server-side drift (a same-size replacement the ETag check can't see) is
// caught by the sampled server-side compare. Legacy files without the map
// fall back to that server-side compare for everything.
type ControlFile struct {
	// Core fields (v0.1.0)
	URL       string  `json:"url"`
	FinalURL  string  `json:"final_url"` // post-redirect URL actually used
	TotalSize int64   `json:"total_size"`
	ChunkSize int64   `json:"chunk_size"`
	ETag      string  `json:"etag,omitempty"`
	Completed []int64 `json:"completed"` // sorted chunk-byte-offsets already written

	// Extended fields (v0.2.0+) — all omitempty for backward compat with v0.1.0 files
	CreatedAt   time.Time `json:"created_at,omitempty"`  // when this control file was first written
	UpdatedAt   time.Time `json:"updated_at,omitempty"`  // last checkpoint timestamp
	Connections int       `json:"connections,omitempty"` // parallel connections used
	UserAgent   string    `json:"user_agent,omitempty"`  // UA sent to server
	ODMVersion  string    `json:"odm_version,omitempty"` // version that created this file
	Checksum    string    `json:"checksum,omitempty"`    // "algo:hex" if --checksum was used

	// ChunkHashes: per-chunk SHA-256 digests for resume verification.
	// Key = chunk Start byte offset, value = lowercase hex sha256 of that
	// chunk's bytes. JSON map keys marshal as strings, so map[int64]string
	// round-trips cleanly. Missing map (nil) = legacy control file; resume
	// falls back to the server-side sample compare.
	ChunkHashes map[int64]string `json:"chunk_hashes,omitempty"`
}

// NoControlFile is returned by LoadControl when the `.odm` file is absent.
var NoControlFile = errors.New("no control file")

// ControlPath returns the `.odm` path that accompanies a destination file.
func ControlPath(destPath string) string { return destPath + ".odm" }

// LoadControl reads and decodes the control file for destPath. Missing file →
// NoControlFile (distinct from a corrupt file, which is a real error). Caller
// decides whether to proceed fresh or abort based on `--continue`.
func LoadControl(destPath string) (*ControlFile, error) {
	b, err := os.ReadFile(ControlPath(destPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, NoControlFile
		}
		return nil, fmt.Errorf("read control: %w", err)
	}
	var cf ControlFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("parse control: %w", err)
	}
	return &cf, nil
}

// SaveControl writes the control file atomically (temp + rename) so a crash
// mid-write can't leave a half-encoded file.
func SaveControl(destPath string, cf *ControlFile) error {
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	tmp := ControlPath(destPath) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write control tmp: %w", err)
	}
	if err := os.Rename(tmp, ControlPath(destPath)); err != nil {
		return fmt.Errorf("rename control: %w", err)
	}
	return nil
}

// RemoveControl deletes the control file (called once a task verifies
// successfully, §12 step 8). Missing-file is not an error.
func RemoveControl(destPath string) error {
	if err := os.Remove(ControlPath(destPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Also remove a stray .tmp from an interrupted save attempt.
	_ = os.Remove(ControlPath(destPath) + ".tmp")
	return nil
}

// CompletedOffsets builds the sorted set of completed chunk byte-offsets for
// fast membership tests at resume time. We key chunks by their *starting*
// byte offset, which is unique and bounded by total size / chunk size.
func (cf *ControlFile) CompletedOffsets() map[int64]struct{} {
	m := make(map[int64]struct{}, len(cf.Completed))
	for _, off := range cf.Completed {
		m[off] = struct{}{}
	}
	return m
}

// ChunkHash returns the recorded hash for the chunk starting at `start`, if
// present. Reading a nil map is safe — legacy files simply report not-found.
func (cf *ControlFile) ChunkHash(start int64) (string, bool) {
	sum, ok := cf.ChunkHashes[start]
	return sum, ok
}
