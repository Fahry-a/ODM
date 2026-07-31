package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// serveFlakyChunk is a range-capable server that fails the first `failTimes`
// requests for the chunk starting at failStart (always 500), then serves
// normally. Used to force chunk failures and resume interruption.
func serveFlakyChunk(t *testing.T, payload []byte, failStart int64, failTimes int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	left := failTimes
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		start, end, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if !ok {
			start, end = 0, int64(len(payload))-1
		}
		if start == failStart {
			mu.Lock()
			if left > 0 {
				left--
				mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Unlock()
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
}

// captureLog wraps a LogFn and records every formatted message.
func captureLog() (downloadLogFn, *[]string, *sync.Mutex) {
	var msgs []string
	var mu sync.Mutex
	fn := func(level, format string, args ...any) {
		mu.Lock()
		msgs = append(msgs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	return fn, &msgs, &mu
}

type downloadLogFn = func(level string, format string, args ...any)

// TestWorker_RequeuesFailedChunk pins the chunk re-queue behaviour: a chunk
// whose requests keep failing within one worker's retry budget is put back in
// the queue instead of failing the whole task, so a transient server error on
// one range doesn't abort the download. The failure counter is per-chunk, so
// the retry is bounded (maxWorkerAttempts = opts.Retry).
func TestWorker_RequeuesFailedChunk(t *testing.T) {
	payload := make([]byte, 3*1024) // 3 chunks of 1 KiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	// Fail the middle chunk for a full internal retry budget (2 requests with
	// Retry=1), then serve it normally. The worker must requeue and complete.
	srv := serveFlakyChunk(t, payload, 1024, 2)
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	var warned atomic.Bool
	logf := func(level, format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "requeued") {
			warned.Store(true)
		}
	}
	task := NewTask(TaskID("odm-requeue"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      1,
		RetryWait:  5 * time.Millisecond,
		ChunkSize:  1024,
		Timeout:    10 * time.Second,
	}, cli, lim, logf)

	if err := task.Start(context.Background(), 1, nil); err != nil {
		t.Fatalf("download with one transiently-failing chunk should succeed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch after requeue (len %d want %d)", len(got), len(payload))
	}
	if !warned.Load() {
		t.Fatalf("expected a requeue warning to be logged")
	}
}

// TestResume_DetectsStaleData pins the resume integrity check: when an
// interrupted download is resumed but the on-disk bytes of a supposedly
// completed chunk no longer match the server, the engine must re-download from
// scratch instead of resuming into a corrupt file.
func TestResume_DetectsStaleData(t *testing.T) {
	payload := make([]byte, 4*1024) // 4 chunks of 1 KiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	failSrv := serveFlakyChunk(t, payload, 3*1024, 1<<30) // chunk 3 always fails
	defer failSrv.Close()
	goodSrv := serveRangeServer(t, payload)
	defer goodSrv.Close()

	dir := t.TempDir()
	newMgr := func(server string, logf downloadLogFn) *Manager {
		t.Helper()
		m, err := NewManager(ExecOptions{
			Dir:         dir,
			OutFile:     "out.bin",
			Connections: 1,
			Retry:       0,
			RetryWait:   5 * time.Millisecond,
			Continue:    true,
			ChunkSize:   1024,
			Timeout:     5 * time.Second,
			MaxRedirect: 3,
			CheckCert:   true,
		}, logf)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}

	// Pass 1: chunk 3 permanently fails → task errors, control file persists
	// with chunks 0..2 completed.
	if err := newMgr(failSrv.URL, nil).Run(context.Background(), failSrv.URL, 1); err == nil {
		t.Fatalf("pass 1 should fail on the always-failing chunk")
	}
	path := filepath.Join(dir, "out.bin")
	if _, err := os.Stat(path + ".odm"); err != nil {
		t.Fatalf("expected control file after interrupted download: %v", err)
	}

	// Corrupt one byte inside the completed chunk 1 region.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[1500] ^= 0xFF
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// Pass 2: good server, resume enabled. Integrity check must catch the
	// corrupt chunk and re-download everything.
	logf, msgs, mu := captureLog()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := newMgr(goodSrv.URL, logf).Run(ctx, goodSrv.URL, 1); err != nil {
		t.Fatalf("pass 2 should succeed after re-download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("final content mismatch (len %d want %d)", len(got), len(payload))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range *msgs {
		if strings.Contains(m, "resume integrity check failed") {
			return
		}
	}
	t.Fatalf("expected a resume integrity warning, logs: %v", *msgs)
}

// TestResume_LayoutMismatchFallsBackToFullDownload pins the control-file layout
// guard: a control file from a ranged download (several completed chunks) that
// later points at a single-stream URL (one whole-file chunk) is incompatible —
// trusting the ranged offsets would mark the file complete on possibly-stale
// data. The engine must detect the mismatch and re-download from scratch.
func TestResume_LayoutMismatchFallsBackToFullDownload(t *testing.T) {
	payload := make([]byte, 4*1024) // 4 chunks of 1 KiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	failSrv := serveFlakyChunk(t, payload, 3*1024, 1<<30)
	defer failSrv.Close()
	noRangeSrv := serveNoRangeServer(t, payload)
	defer noRangeSrv.Close()

	dir := t.TempDir()
	newMgr := func(server string, logf downloadLogFn) *Manager {
		t.Helper()
		m, err := NewManager(ExecOptions{
			Dir:         dir,
			OutFile:     "out.bin",
			Connections: 1,
			Retry:       0,
			RetryWait:   5 * time.Millisecond,
			Continue:    true,
			ChunkSize:   1024,
			Timeout:     5 * time.Second,
			MaxRedirect: 3,
			CheckCert:   true,
		}, logf)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}

	if err := newMgr(failSrv.URL, nil).Run(context.Background(), failSrv.URL, 1); err == nil {
		t.Fatalf("pass 1 should fail on the always-failing chunk")
	}

	logf, msgs, mu := captureLog()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := newMgr(noRangeSrv.URL, logf).Run(ctx, noRangeSrv.URL, 1); err != nil {
		t.Fatalf("pass 2 should succeed via single-stream re-download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch (len %d want %d)", len(got), len(payload))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range *msgs {
		if strings.Contains(m, "control file layout doesn't match") {
			return
		}
	}
	t.Fatalf("expected a layout-mismatch warning, logs: %v", *msgs)
}

// TestResume_VerifyPassesIntactData pins the positive resume path: intact
// completed chunks pass the integrity check, so the download resumes (only the
// missing chunk is fetched) instead of re-downloading everything.
func TestResume_VerifyPassesIntactData(t *testing.T) {
	payload := make([]byte, 4*1024) // 4 chunks of 1 KiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	failSrv := serveFlakyChunk(t, payload, 3*1024, 1<<30)
	defer failSrv.Close()
	goodSrv := serveRangeServer(t, payload)
	defer goodSrv.Close()

	dir := t.TempDir()
	newMgr := func(server string, logf downloadLogFn) *Manager {
		t.Helper()
		m, err := NewManager(ExecOptions{
			Dir:         dir,
			OutFile:     "out.bin",
			Connections: 1,
			Retry:       0,
			RetryWait:   5 * time.Millisecond,
			Continue:    true,
			ChunkSize:   1024,
			Timeout:     5 * time.Second,
			MaxRedirect: 3,
			CheckCert:   true,
		}, logf)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}

	if err := newMgr(failSrv.URL, nil).Run(context.Background(), failSrv.URL, 1); err == nil {
		t.Fatalf("pass 1 should fail on the always-failing chunk")
	}

	logf, msgs, mu := captureLog()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := newMgr(goodSrv.URL, logf).Run(ctx, goodSrv.URL, 1); err != nil {
		t.Fatalf("pass 2 resume should succeed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("final content mismatch (len %d want %d)", len(got), len(payload))
	}
	mu.Lock()
	defer mu.Unlock()
	sawResume, sawVerify := false, false
	for _, m := range *msgs {
		if strings.Contains(m, "resuming") {
			sawResume = true
		}
		if strings.Contains(m, "resume integrity check failed") {
			sawVerify = true
		}
	}
	if !sawResume {
		t.Fatalf("expected the resume path to be taken, logs: %v", *msgs)
	}
	if sawVerify {
		t.Fatalf("intact data must pass the integrity check, logs: %v", *msgs)
	}
}
