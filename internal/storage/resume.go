package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ControlFile is the JSON payload of `<filename>.odm` (PRD §11.3). It records
// enough state to resume: the source URL (to revalidate the server hasn't
// shipped a different file via ETag/Content-Length drift — kept but not
// enforced strictly in MVP), the total size, chunk size, and the set of chunk
// indices already written. On a resume we re-queue only the missing chunks.
type ControlFile struct {
	URL       string  `json:"url"`
	FinalURL  string  `json:"final_url"` // post-redirect URL actually used
	TotalSize int64   `json:"total_size"`
	ChunkSize int64   `json:"chunk_size"`
	ETag      string  `json:"etag,omitempty"`
	Completed []int64 `json:"completed"` // sorted chunk-byte-offsets already written
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

// DirOf is a small helper used by callers that build destPath via filepath.Join.
func DirOf(p string) string { return filepath.Dir(p) }
