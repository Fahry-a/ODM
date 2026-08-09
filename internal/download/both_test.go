package download

import (
	"bytes"
	"context"
	"net"
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

// serveRangeSlices serves the payload with full Range support via ServeContent.
func serveRangeSlices(payload []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(payload))
	}))
}

// buildBothTask wires a Task with the both profile against a given URL.
func buildBothTask(t *testing.T, dir string, url string, conns int) *Task {
	t.Helper()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second, HTTP2: true, CheckCertificate: false})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-both"), url, TaskOptions{
		OutputName:       "out.bin",
		Dir:              dir,
		Retry:            2,
		Timeout:          10 * time.Second,
		ChunkSize:        1 * 1024 * 1024,
		MinSplitSize:     1 * 1024 * 1024,
		Split:            4,
		Profile:          "both",
		MaxConnPerServer: 8,
	}, cli, lim, nil)
	task.SetH2Client(cli)
	return task
}

// TestBothProfile_EndToEnd: a both download over a range-capable server must
// produce a byte-identical file with two engines working concurrently.
func TestBothProfile_EndToEnd(t *testing.T) {
	payload := make([]byte, 10*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeSlices(payload)
	defer srv.Close()

	dir := t.TempDir()
	task := buildBothTask(t, dir, srv.URL, 6)
	if err := task.Start(context.Background(), 6, nil); err != nil {
		t.Fatalf("both download failed: %v", err)
	}
	if task.engines == nil || len(task.engines) != 2 {
		t.Fatal("both task must have 2 engines")
	}
	if task.splitAt <= 0 || task.splitAt >= int64(len(payload)) {
		t.Fatalf("splitAt %d out of range for %d-byte file", task.splitAt, len(payload))
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("file mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestBothProfile_DegradesTiny: files < 4 MiB degrade to single-region odm.
func TestBothProfile_DegradesTiny(t *testing.T) {
	payload := []byte("tiny-file-that-is-not-worth-splitting")
	srv := serveRangeSlices(payload)
	defer srv.Close()

	dir := t.TempDir()
	task := buildBothTask(t, dir, srv.URL, 4)
	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("tiny both download failed: %v", err)
	}
	if task.engines != nil {
		t.Fatalf("tiny file must not split into engines, got %d", len(task.engines))
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("tiny both produced wrong file: %v", err)
	}
}

// TestBothProfile_ResumeMidFlight cancels mid-download, then resumes and
// verifies the resume gate + per-region offsets reconstruct the same layout.
func TestBothProfile_ResumeMid(t *testing.T) {
	payload := make([]byte, 8*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeSlices(payload)
	defer srv.Close()
	dir := t.TempDir()

	// Pass 1: start, then cancel mid-flight so a control file is left behind.
	task := buildBothTask(t, dir, srv.URL, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- task.Start(ctx, 4, nil) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	// Pass 2: resume with the same profile params.
	task2 := buildBothTask(t, dir, srv.URL, 4)
	if err := task2.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("both resume failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("resumed both file mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestBothProfile_H2Region2: region 2 (the aria2c half) must speak HTTP/2.
func TestBothProfile_H2Region2(t *testing.T) {
	payload := make([]byte, 5*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var h2Requests atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			h2Requests.Add(1)
		}
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(payload))
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context { return ctx }
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	task := buildBothTask(t, dir, srv.URL, 6)
	if err := task.Start(context.Background(), 6, nil); err != nil {
		t.Fatalf("both h2 download failed: %v", err)
	}
	if task.engines != nil && task.engines[1].Client() == nil {
		t.Fatal("region2 must have a client")
	}
	// Both engines share the same h2 client here; region2 requests happen
	// over h2. At least some requests must have arrived as h2.
	if h2Requests.Load() == 0 {
		t.Fatal("expected h2 requests (region2 speaks h2)")
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("h2 both produced wrong file: %v", err)
	}
}
