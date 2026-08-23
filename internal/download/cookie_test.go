package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// TestCookieHeader_ReachesServer pins the --load-cookies/-H pipeline
// end-to-end: the Cookie header rides the transport client into every ranged
// GET — and never leaks into the .odm control file (it can carry credentials).
func TestCookieHeader_ReachesServer(t *testing.T) {
	var sawCookie atomic.Bool
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.Header.Get("Cookie"), "sid=SECRET1") {
			sawCookie.Store(true)
		}
		start, end, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{
		Timeout: 5 * time.Second,
		Headers: []string{"Cookie: sid=SECRET1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t"), srv.URL, TaskOptions{
		OutputName: "out.bin", Dir: dir, Retry: 2,
		RetryWait: time.Millisecond, Timeout: 5 * time.Second, ChunkSize: 1024,
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("odm")
	if err := task.Start(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	if !sawCookie.Load() {
		t.Fatal("server never saw the Cookie header")
	}

	// The control file must NOT carry the header value.
	if b, rerr := os.ReadFile(filepath.Join(dir, "out.bin.odm")); rerr == nil {
		if strings.Contains(string(b), "SECRET1") {
			t.Fatal("cookie leaked into the control file")
		}
	}
}
