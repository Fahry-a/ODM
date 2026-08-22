package download

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// newRangeIgnoreTask builds a single-connection task against srv with a
// pre-set probe, so the test exercises exactly the multi-chunk write path
// (no extra probe traffic competing for the server's "first good response").
func newRangeIgnoreTask(t *testing.T, srvURL string, dir string) *Task {
	t.Helper()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	return NewTask(TaskID("odm-range-ignore"), srvURL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      2,
		Timeout:    10 * time.Second,
		ChunkSize:  3 * 1024 * 1024,
	}, cli, lim, nil)
}

// setProbeAndStart pins the probe so Start skips its own and starts the
// single worker immediately.
func setProbeAndStart(t *testing.T, task *Task, url string, total int64) error {
	t.Helper()
	task.SetProbe(&transport.ProbeResult{
		FinalURL:      url,
		SupportsRange: true,
		TotalSize:     total,
		Filename:      "out.bin",
	})
	task.SetProfile("odm")
	return task.Start(context.Background(), 1, nil)
}

// serveCorrectOnceThenFull honours the FIRST ranged GET with the exact
// requested slice (206 + truthful Content-Range); every later ranged GET gets
// 200 + the full body — a server that stops honouring Range mid-download.
// Before the guards in fetchAndWrite, such a response was written at the
// chunk's offset, silently corrupting the file.
func serveCorrectOnceThenFull(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	firstOK := &atomic.Bool{}
	firstOK.Store(true)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" && firstOK.CompareAndSwap(true, false) {
			start, end, ok := parseRangeHeader(r.Header.Get("Range"))
			if !ok {
				start, end = 0, int64(len(payload)-1)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start : end+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
}

// serveLying206 answers every ranged GET with a 206 whose Content-Range
// claims the data starts 1 MiB into the resource while actually sending
// payload[1MiB:] — data positioned wrong relative to the request. Only the
// Content-Range check in fetchAndWrite keeps this off disk.
func serveLying206(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	const lie = 1 << 20
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lie, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[lie:])
	}))
}

// parseRangeHeader extracts (start, end) from "bytes=S-E".
func parseRangeHeader(h string) (int64, int64, bool) {
	rest, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, 0, false
	}
	sStr, eStr, _ := strings.Cut(rest, "-")
	start, err1 := strconv.ParseInt(sStr, 10, 64)
	end, err2 := strconv.ParseInt(eStr, 10, 64)
	return start, end, err1 == nil && err2 == nil
}

// TestRangeIgnoreMidDownload_DoesNotCorrupt pins the mid-flight corruption fix:
// when a ranged request comes back 200 (server dropped range support for that
// request), the chunk must be retried as a transient error — never written.
// The one honest 206 must land at ITS OWN offset; everything else stays an
// unwritten hole.
func TestRangeIgnoreMidDownload_DoesNotCorrupt(t *testing.T) {
	payload := make([]byte, 9*1024*1024)
	for i := range payload {
		payload[i] = byte(i%253 + 1) // never 0: holes are unambiguous
	}
	srv := serveCorrectOnceThenFull(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	task := newRangeIgnoreTask(t, srv.URL, dir)
	err := setProbeAndStart(t, task, srv.URL, int64(len(payload)))
	if err == nil {
		t.Fatal("expected failure once the server stops honouring ranges")
	}

	got, rerr := os.ReadFile(filepath.Join(dir, "out.bin"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(got) != len(payload) {
		t.Fatalf("file is %d bytes, want %d", len(got), len(payload))
	}
	const chunk0 = 3 * 1024 * 1024
	for i := 0; i < chunk0; i++ {
		if got[i] != payload[i] {
			t.Fatalf("honest 206 chunk corrupted at offset %d", i)
		}
	}
	for i := chunk0; i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("stray byte at offset %d (full-body 200 written into a chunk slot)", i)
		}
	}
}

// TestLyingContentRange_DoesNotCorrupt pins the Content-Range guard: a 206
// whose declared start mismatches the requested offset must never touch the
// file.
func TestLyingContentRange_DoesNotCorrupt(t *testing.T) {
	payload := make([]byte, 9*1024*1024)
	for i := range payload {
		payload[i] = byte(i%253 + 1)
	}
	srv := serveLying206(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	task := newRangeIgnoreTask(t, srv.URL, dir)
	err := setProbeAndStart(t, task, srv.URL, int64(len(payload)))
	if err == nil {
		t.Fatal("expected failure against a lying Content-Range server")
	}

	got, rerr := os.ReadFile(filepath.Join(dir, "out.bin"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("misplaced 206 wrote a byte at offset %d", i)
		}
	}
}
