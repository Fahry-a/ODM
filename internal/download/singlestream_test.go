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

// serveNoRangeServer reports a size (Content-Length) but ignores every Range
// request and always serves 200 with the full body — the single-stream
// fallback case. The probe classifies it SupportsRange=false, SingleStream=true,
// TotalSize=<len(payload)>.
func serveNoRangeServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoaS(len(payload)))
		_, _ = w.Write(payload)
	}))
}

// TestSingleStream_SizedServerIgnoresRange is a regression test for a data
// corruption bug: when the server reports the file size but does not support
// ranges, the engine used to build a multi-chunk queue and special-case only
// chunk 0 as a plain GET. Chunk 1+ then issued ranged GETs the server answered
// with the FULL body, which was written at the chunk's offset (14 MB file for a
// 10 MB payload) AND over-counted bytesDone past TotalSize — which flipped the
// task into StateCompleted, deleted the resume control file, and reported
// success on the corrupt output. The fix is a single whole-file chunk for any
// SingleStream download; this test pins that: the file must be byte-identical
// and the task must complete cleanly even when Start is given extra workers.
func TestSingleStream_SizedServerIgnoresRange(t *testing.T) {
	payload := make([]byte, 10*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveNoRangeServer(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-single"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      2,
		Timeout:    10 * time.Second,
		ChunkSize:  4 * 1024 * 1024,
	}, cli, lim, nil)

	// Pass a connection budget > 1 to prove extra workers can't re-enable the
	// corruption (the single whole-file chunk leaves nothing else to pull).
	if err := task.Start(context.Background(), 3, nil); err != nil {
		t.Fatalf("single-stream download should succeed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("file size %d, want %d (corrupt)", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("corrupt at offset %d: got %d want %d", i, got[i], payload[i])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out.bin.odm")); !os.IsNotExist(err) {
		t.Fatalf("control file should be removed after a clean completion")
	}
}
