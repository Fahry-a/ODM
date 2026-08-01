package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestControlPath confirms the `.odm` suffix convention used by callers.
func TestControlPath(t *testing.T) {
	cases := map[string]string{
		"/tmp/blob.bin": "/tmp/blob.bin.odm",
		"rel.tar":       "rel.tar.odm",
		"/x/y.z/file":   "/x/y.z/file.odm",
		"":              ".odm",
	}
	for in, want := range cases {
		if got := ControlPath(in); got != want {
			t.Errorf("ControlPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestControlFile_CompletedOffsets builds the membership set from the saved
// offsets — resume uses this for "is chunk X already done?" lookup.
func TestControlFile_CompletedOffsets(t *testing.T) {
	cf := &ControlFile{Completed: []int64{0, 4096, 8192}}
	m := cf.CompletedOffsets()
	if len(m) != 3 {
		t.Fatalf("len = %d, want 3", len(m))
	}
	for _, k := range []int64{0, 4096, 8192} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing offset %d", k)
		}
	}
	if _, ok := m[1234]; ok {
		t.Errorf("spurious offset 1234 present")
	}
}

// TestSaveLoadControl_RoundTrip verifies Save → Load produces an equivalent
// ControlFile — the resume path's correctness depends on this fidelity.
func TestSaveLoadControl_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "file.tar.gz")
	in := &ControlFile{
		URL:       "https://example.com/file.tar.gz",
		FinalURL:  "https://cdn.example.com/file.tar.gz",
		TotalSize: 1 << 20,
		ChunkSize: 4 << 10,
		ETag:      `"deadbeef"`,
		Completed: []int64{0, 4096, 8192, 12288},
	}
	if err := SaveControl(dest, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := os.Stat(ControlPath(dest)); err != nil {
		t.Fatalf("control file not written: %v", err)
	}
	out, err := LoadControl(dest)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch\nin  = %+v\nout = %+v", in, out)
	}
}

// TestLoadControl_MissingFile: the §11.3 contract distinguishes "no control
// file" (fresh download / resume-from-scratch) from "control file corrupt".
// The first must return sentinel NoControlFile so callers can branch cleanly.
func TestLoadControl_MissingFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "missing.bin")
	cf, err := LoadControl(dest)
	if !errors.Is(err, NoControlFile) {
		t.Fatalf("want NoControlFile, got cf=%v err=%v", cf, err)
	}
	if cf != nil {
		t.Fatalf("want nil cf for missing control file")
	}
}

// TestLoadControl_CorruptFile surfaces JSON corruption as a real error (NOT
// NoControlFile) so the engine can decide whether to abort or restart fresh.
func TestLoadControl_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "broken.tar")
	if err := os.WriteFile(ControlPath(dest), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadControl(dest); err == nil {
		t.Fatalf("want error for corrupt control file")
	}
}

// TestSaveControl_AtomicWritesJSON: the file must contain parseable JSON with
// the expected fields, and must be indented for human readability on crash
// inspection. (Indent is a soft guarantee; the hard guarantee is "valid JSON
// that LoadControl can parse" — exercised by the round-trip test above.)
func TestSaveControl_AtomicWritesJSON(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	cf := &ControlFile{URL: "u", TotalSize: 10, ChunkSize: 2, Completed: []int64{0}}
	if err := SaveControl(dest, cf); err != nil {
		t.Fatalf("save: %v", err)
	}
	b, err := os.ReadFile(ControlPath(dest))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("not parseable JSON: %v", err)
	}
	if raw["url"] != "u" || raw["total_size"] != float64(10) {
		t.Fatalf("unexpected json: %v", raw)
	}
	// No ETag set → omitempty should drop the key entirely.
	if _, ok := raw["etag"]; ok {
		t.Fatalf("etag should be omitempty'd")
	}
}

// TestSaveControl_NoLeftoverTmp: a successful save must not leave the .tmp
// file behind (the rename is atomic, so either the old or new version is on
// disk — never a half-written tmp).
func TestSaveControl_NoLeftoverTmp(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := SaveControl(dest, &ControlFile{URL: "u"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(ControlPath(dest) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover after save (err=%v)", err)
	}
}

// TestSaveLoadControl_ChunkHashesRoundTrip verifies that per-chunk hashes
// survive a Save → Load cycle — the resume-verification path depends on this
// fidelity. JSON map keys marshal as strings and unmarshal back to int64.
func TestSaveLoadControl_ChunkHashesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "hashed.bin")
	in := &ControlFile{
		URL:       "https://example.com/hashed.bin",
		TotalSize: 1024,
		ChunkSize: 256,
		Completed: []int64{0, 256},
		ChunkHashes: map[int64]string{
			0:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
	if err := SaveControl(dest, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadControl(dest)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(out.ChunkHashes, in.ChunkHashes) {
		t.Fatalf("ChunkHashes round-trip mismatch\nin  = %v\nout = %v", in.ChunkHashes, out.ChunkHashes)
	}
	if !reflect.DeepEqual(out.Completed, in.Completed) {
		t.Fatalf("Completed round-trip mismatch\nin  = %v\nout = %v", in.Completed, out.Completed)
	}
}

// TestLoadControl_LegacyWithoutChunkHashes: a v0.x-era control file that has no
// chunk_hashes key must load with a nil map (legacy), leaving resume to fall
// back to the server-side sample compare, and must not disturb Completed.
func TestLoadControl_LegacyWithoutChunkHashes(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "legacy.bin")
	legacy := `{
  "url": "https://example.com/legacy.bin",
  "total_size": 1024,
  "chunk_size": 256,
  "completed": [0, 256]
}`
	if err := os.WriteFile(ControlPath(dest), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cf, err := LoadControl(dest)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cf.ChunkHashes != nil {
		t.Fatalf("legacy file should load with nil ChunkHashes, got %v", cf.ChunkHashes)
	}
	if !reflect.DeepEqual(cf.Completed, []int64{0, 256}) {
		t.Fatalf("Completed = %v, want [0 256]", cf.Completed)
	}
}

// TestControlFile_ChunkHashHelpers pins ChunkHash: exact key round-trip, and
// safe read on a nil (legacy) map.
func TestControlFile_ChunkHashHelpers(t *testing.T) {
	var cf ControlFile
	if _, ok := cf.ChunkHash(0); ok {
		t.Fatalf("legacy nil map must report not-found")
	}
	cf.ChunkHashes = map[int64]string{4096: "deadbeef"}
	if got, ok := cf.ChunkHash(4096); !ok || got != "deadbeef" {
		t.Fatalf("ChunkHash(4096) = %q, %v; want deadbeef, true", got, ok)
	}
	if _, ok := cf.ChunkHash(8192); ok {
		t.Fatalf("unset offset must report not-found")
	}
}

// TestSaveControl_OmitsEmptyChunkHashes: omitempty must drop a nil hash map
// from the JSON entirely so new files stay backward-compatible with readers
// that predate the field.
func TestSaveControl_OmitsEmptyChunkHashes(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	cf := &ControlFile{URL: "u", TotalSize: 10, ChunkSize: 2, Completed: []int64{0}}
	if err := SaveControl(dest, cf); err != nil {
		t.Fatalf("save: %v", err)
	}
	b, err := os.ReadFile(ControlPath(dest))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("not parseable JSON: %v", err)
	}
	if _, ok := raw["chunk_hashes"]; ok {
		t.Fatalf("chunk_hashes should be omitempty'd when nil, got: %v", raw)
	}
}

// TestRemoveControl_Idempotent: deleting twice (or deleting a non-existent
// file) must succeed — the engine calls this defensively on completion.
func TestRemoveControl_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ghost.bin")
	if err := RemoveControl(dest); err != nil {
		t.Fatalf("remove missing: %v", err)
	}

	// Now create one and remove twice.
	if err := SaveControl(dest, &ControlFile{URL: "u"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := RemoveControl(dest); err != nil {
		t.Fatalf("remove existing: %v", err)
	}
	if err := RemoveControl(dest); err != nil {
		t.Fatalf("remove after delete: %v", err)
	}
}

// TestRemoveControl_CleansStrayTmp: a crashed SaveControl can leave a .tmp
// behind; RemoveControl also sweeps it so the next download starts clean.
func TestRemoveControl_CleansStrayTmp(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(ControlPath(dest)+".tmp", []byte("partial"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := RemoveControl(dest); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(ControlPath(dest) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp not swept (err=%v)", err)
	}
}
