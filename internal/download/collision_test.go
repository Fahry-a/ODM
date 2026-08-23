package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// newCollisionTask builds a single-conn task with the given collision mode.
func newCollisionTask(t *testing.T, srvURL, dir, collision string) *Task {
	t.Helper()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	return NewTask(TaskID("t"), srvURL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      2,
		RetryWait:  time.Millisecond,
		Timeout:    5 * time.Second,
		ChunkSize:  1024,
		Collision:  collision,
	}, cli, lim, nil)
}

func startCollisionTask(t *testing.T, task *Task, url string, total int64) error {
	t.Helper()
	task.SetProbe(&transport.ProbeResult{FinalURL: url, SupportsRange: true, TotalSize: total, Filename: "out.bin"})
	task.SetProfile("odm")
	return task.Start(context.Background(), 1, nil)
}

func serveFixedPayload(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		start, end, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

func payload4K() []byte {
	p := make([]byte, 4096)
	for i := range p {
		p[i] = byte(i%251 + 1)
	}
	return p
}

// TestCollision_RenamePinsCounter pins --auto-rename semantics: the first
// collision lands at name.1.ext; a second download in the same dir (both
// files now existing) lands at name.2.ext — and every file keeps its bytes.
func TestCollision_Rename(t *testing.T) {
	payload := payload4K()
	srv := serveFixedPayload(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	existing := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"out.1.bin", "out.2.bin"} {
		task := newCollisionTask(t, srv.URL, dir, "rename")
		if err := startCollisionTask(t, task, srv.URL, int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		if got := task.OutputPath(); filepath.Base(got) != want {
			t.Fatalf("saved as %s, want %s", filepath.Base(got), want)
		}
	}
	// The pre-existing file is untouched.
	if b, _ := os.ReadFile(existing); string(b) != "old" {
		t.Fatalf("existing file was overwritten: %q", b)
	}
}

// TestCollision_SkipSizeMatch pins --skip-existing: an exact-size match skips
// without touching the file; a size mismatch re-downloads over it.
func TestCollision_SkipExisting(t *testing.T) {
	payload := payload4K()
	srv := serveFixedPayload(t, payload)
	defer srv.Close()

	dir := t.TempDir()

	// Exact size → skip, file untouched.
	match := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(match, append([]byte(nil), payload...), 0o644); err != nil {
		t.Fatal(err)
	}
	task := newCollisionTask(t, srv.URL, dir, "skip")
	if err := startCollisionTask(t, task, srv.URL, int64(len(payload))); err != nil {
		t.Fatalf("size-matched skip should succeed: %v", err)
	}
	if st, _ := os.Stat(match); st == nil || st.Size() != int64(len(payload)) {
		t.Fatal("skipped file was modified")
	}

	// Wrong size → re-download (overwrite path).
	if err := os.WriteFile(match, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	task = newCollisionTask(t, srv.URL, dir, "skip")
	if err := startCollisionTask(t, task, srv.URL, int64(len(payload))); err != nil {
		t.Fatalf("size mismatch must re-download: %v", err)
	}
	got, _ := os.ReadFile(match)
	if len(got) != len(payload) {
		t.Fatalf("re-download produced %d bytes, want %d", len(got), len(payload))
	}
}

// TestUniqueName pins the counter placement: filepath.Ext's last extension,
// so "f.tar.gz" → "f.tar.1.gz" and "f.bin" → "f.1.bin".
func TestUniqueName(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) {
		if werr := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); werr != nil {
			t.Fatal(werr)
		}
	}

	mk("f.tar.gz")
	if got := uniqueName(dir, "f.tar.gz"); got != "f.tar.1.gz" {
		t.Fatalf("f.tar.gz -> %s, want f.tar.1.gz (counter before Ext)", got)
	}
	if got := uniqueName(dir, "f.bin"); got != "f.1.bin" {
		t.Fatalf("f.bin -> %s, want f.1.bin", got)
	}
	// Fill 1..3, next must be 4.
	for i := 1; i <= 3; i++ {
		mk(string(rune('a'+i-1)) + ".bin") // placeholder to keep dir non-empty
	}
	if rerr := os.Remove(filepath.Join(dir, "f.bin")); rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	for _, n := range []string{"f.1.bin", "f.2.bin"} {
		mk(n)
	}
	if got := uniqueName(dir, "f.bin"); got != "f.3.bin" {
		t.Fatalf("third free slot -> %s, want f.3.bin", got)
	}
}

// TestParseChecksumSidecar pins the sidecar digest parser: bare hashes by
// length, algo-prefixed forms, sha256sum output with filename, and rejects.
func TestParseChecksumSidecar(t *testing.T) {
	h64 := strings.Repeat("a", 64)
	h40 := strings.Repeat("b", 40)
	h32 := strings.Repeat("c", 32)
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{h64, "sha256:" + h64, false},
		{"SHA256:" + h64 + "  file.zip\n", "sha256:" + h64, false},
		{h40, "sha1:" + h40, false},
		{h32, "md5:" + h32, false},
		{"sha256:" + h64, "sha256:" + h64, false},
		{"", "", true},
		{"zzz", "", true},
		{"crc32:" + h32, "", true},
	}
	for _, tc := range cases {
		got, err := parseChecksumSidecar(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parse(%q) should fail", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parse(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
