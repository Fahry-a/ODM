package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// serveFlakyRangeServer honours the FIRST ranged GET per connection (206) but
// answers every SUBSEQUENT one with 200 + the full body — a server that stops
// honouring Range mid-download. Before the 200-vs-206 guard in fetchAndWrite,
// such a response was written at the chunk's offset, silently corrupting the
// file (chunk N's slot holding bytes from position 0).
func serveFlakyRangeServer(t *testing.T, payload []byte, firstOK *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" && firstOK.CompareAndSwap(true, false) {
			w.Header().Set("Content-Range", "bytes 0-"+itoaS(len(payload)-1)+"/"+itoaS(len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			w.Header().Set("Content-Length", itoaS(len(payload)))
			_, _ = w.Write(payload)
			return
		}
		w.Header().Set("Content-Length", itoaS(len(payload)))
		_, _ = w.Write(payload)
	}))
}

// TestRangeIgnoreMidDownload_DoesNotCorrupt pins the mid-flight corruption fix:
// when a ranged request comes back 200 (server dropped range support for that
// request), the chunk must be retried as a transient error — never written.
func TestRangeIgnoreMidDownload_DoesNotCorrupt(t *testing.T) {
	payload := make([]byte, 9*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 253)
	}
	firstOK := &atomic.Bool{}
	firstOK.Store(true)
	srv := serveFlakyRangeServer(t, payload, firstOK)
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-range-ignore"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      3,
		Timeout:    10 * time.Second,
		ChunkSize:  3 * 1024 * 1024,
	}, cli, lim, nil)

	pr := &transport.ProbeResult{
		FinalURL:      srv.URL,
		SupportsRange: true,
		TotalSize:     int64(len(payload)),
		Filename:      "out.bin",
	}
	task.SetProbe(pr)
	task.SetProfile("odm")

	err = task.Start(context.Background(), 2, nil)
	if err == nil {
		t.Fatal("expected the task to fail once the server stops honouring ranges and retries are exhausted")
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "out.bin"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	// Whatever is on disk must be intact chunks only: any byte written from a
	// full-body 200 would place payload[0..] at a non-zero offset. Check the
	// first chunk boundary: if the file has data past chunk 0's end there, it
	// must match the payload there (a corrupt write would not).
	for i := 0; i < len(got) && i < len(payload); i++ {
		if got[i] != payload[i] {
			t.Fatalf("corruption at offset %d: got %d want %d", i, got[i], payload[i])
		}
	}
	if len(got) > len(payload) {
		t.Fatalf("file grew to %d bytes, want ≤ %d (full body written into a chunk slot)", len(got), len(payload))
	}
}
