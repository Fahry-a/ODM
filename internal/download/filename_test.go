package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// TestFlattenFilename pins the single-path-component contract of
// flattenFilename: separators become '_', dot segments collapse to the
// download.bin fallback, ordinary names pass through untouched.
func TestFlattenFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"../../.bashrc", ".._.._.bashrc"}, // separators flattened → cannot escape
		{"..", "download.bin"},
		{".", "download.bin"},
		{"", "download.bin"},
		{"a/b/c.txt", "a_b_c.txt"},
		{"..\\..\\win.txt", ".._.._win.txt"}, // backslashes flattened; name kept
		{"normal.tar.gz", "normal.tar.gz"},
		{"..hidden", "..hidden"}, // trailing-dot names are legal filenames
	}
	for _, c := range cases {
		if got := flattenFilename(c.in); got != c.want {
			t.Errorf("flattenFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStart_TraversalFilename proves the end-to-end guard: a server answering
// with a Content-Disposition traversal filename must NOT write outside Dir.
func TestStart_TraversalFilename(t *testing.T) {
	payload := []byte("traversal-payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../escaped"`)
		w.Header().Set("Content-Length", itoaS(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t-trav"), srv.URL, TaskOptions{
		Dir:       dir,
		Retry:     1,
		Timeout:   5 * time.Second,
		ChunkSize: 1024,
	}, cli, lim, nil)
	// Single-stream probe (no range support): the documented contract for a
	// server without Accept-Ranges, so Start takes the whole-file GET path.
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: false, TotalSize: int64(len(payload)),
		SingleStream: true, Filename: "../../escaped"})
	if err := task.Start(context.Background(), 1, nil); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The separators became underscores, so the payload must land at
	// Dir/.._.._escaped — inside the directory, never outside it.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped")); err == nil {
		t.Fatal("filename escaped the destination directory")
	}
	got, err := os.ReadFile(filepath.Join(dir, ".._.._escaped"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("flattened file missing/wrong inside Dir: %v", err)
	}
}
