package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// oversendServer answers every Range request with a truthful 206 +
// Content-Range, but streams `extra` additional bytes after the requested
// range on a chunked connection — the only framing that lets surplus bytes
// reach the application (Content-Length framing would truncate at exactly the
// declared length inside the HTTP transport).
func oversendServer(t *testing.T, payload []byte, extra int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := parseRangeHeader(r.Header.Get("Range"))
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server must support flushing for chunked responses")
			return
		}
		_, _ = w.Write(payload[start : end+1])
		_, _ = w.Write(bytes.Repeat([]byte{0xEE}, extra))
		fl.Flush()
	}))
}

// TestOversendingServer_NoWritesBeyondRange pins the over-read boundary: a
// 206 whose body runs past the requested range must not place a single byte
// beyond the chunk's reserved region. The task fails instead of corrupting
// the file.
func TestOversendingServer_NoWritesBeyondRange(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5A}, 8192)
	srv := oversendServer(t, payload, 4096)
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t-oversend"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      0,
		Timeout:    5 * time.Second,
		ChunkSize:  4096,
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("odm")

	err = task.Start(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("an over-sending server must fail the task, not corrupt the file")
	}

	got, rerr := os.ReadFile(filepath.Join(dir, "out.bin"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if int64(len(got)) != int64(len(payload)) {
		t.Fatalf("file grew past the declared size: %d bytes (want %d)", len(got), len(payload))
	}
	for i, b := range got {
		if b == 0xEE {
			t.Fatalf("surplus byte written at offset %d", i)
		}
	}
}

// TestOversendingSingleStreamServer_KnownSize pins the same boundary for the
// whole-file path with a probed (known) size: surplus bytes are rejected and
// the file never exceeds the probed size.
func TestOversendingSingleStreamServer_KnownSize(t *testing.T) {
	payload := bytes.Repeat([]byte{0x33}, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // no Content-Length: chunked framing
		fl := w.(http.Flusher)
		_, _ = w.Write(payload)
		_, _ = w.Write(bytes.Repeat([]byte{0xEE}, 512))
		fl.Flush()
	}))
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t-oversend-whole"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      0,
		Timeout:    5 * time.Second,
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: false, SingleStream: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("odm")

	err = task.Start(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("an over-sending single-stream server must fail the task")
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "out.bin"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if int64(len(got)) > int64(len(payload)) {
		t.Fatalf("file grew past the probed size: %d bytes", len(got))
	}
	for i, b := range got {
		if b == 0xEE {
			t.Fatalf("surplus byte written at offset %d", i)
		}
	}
}

// TestUnknownSizeDownload_TruncatesStaleTail pins the stale-tail fix: a
// re-download over a longer previous file must end exactly at the new
// stream's length — the bytes beyond it are not the new content.
func TestUnknownSizeDownload_TruncatesStaleTail(t *testing.T) {
	payload := bytes.Repeat([]byte{0x42}, 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	stale := bytes.Repeat([]byte{0x41}, 10000)
	if err := os.WriteFile(filepath.Join(dir, "out.bin"), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t-stale-tail"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      0,
		Timeout:    5 * time.Second,
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: false, SingleStream: true, TotalSize: -1, Filename: "out.bin"})
	task.SetProfile("odm")

	if err := task.Start(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "out.bin"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("stale tail survived: file is %d bytes, want %d", len(got), len(payload))
	}
}
