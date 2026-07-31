package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"odm/internal/storage"
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

// TestTask_CancelBeforeStartFailsFast pins the queued-task cancel fix: RPC
// remove on a task that hasn't started yet used to be a silent no-op (Cancel
// had no ctx to cancel), so the task would still download once a slot freed.
// Cancel now flags the task and Start fails fast without touching the server.
func TestTask_CancelBeforeStartFailsFast(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-cancel"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      0,
		Timeout:    10 * time.Second,
		ChunkSize:  1024,
	}, cli, lim, nil)

	task.Cancel() // simulates RPC remove while the task was still queued
	if err := task.Start(context.Background(), 1, nil); err == nil {
		t.Fatalf("a cancelled task must fail fast, got success")
	}
	if hits.Load() != 0 {
		t.Fatalf("cancelled task must not touch the server (probe/download), got %d requests", hits.Load())
	}
}

// TestTask_ReusesPreProbe pins the single-probe flow: the CLI probes every URL
// once (for the Balancer) and injects the result via SetProbe; Start must then
// skip the network probe (no HEAD, no bytes=0-0 request) and download straight
// from the known size/range verdict.
func TestTask_ReusesPreProbe(t *testing.T) {
	var probeReqs atomic.Int64
	payload := make([]byte, 2048)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || r.Header.Get("Range") == "bytes=0-0" {
			probeReqs.Add(1)
		}
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
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := cli.Probe(context.Background(), srv.URL) // the "main" probe
	if err != nil {
		t.Fatal(err)
	}
	probeReqs.Store(0)

	lim, _ := ratelimit.New("")
	dir := t.TempDir()
	task := NewTask(TaskID("odm-preprobe"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      0,
		Timeout:    10 * time.Second,
		ChunkSize:  1024,
	}, cli, lim, nil)
	task.SetProbe(pr)

	if err := task.Start(context.Background(), 1, nil); err != nil {
		t.Fatalf("download with pre-probe should succeed: %v", err)
	}
	if probeReqs.Load() != 0 {
		t.Fatalf("expected no probe requests after SetProbe, got %d", probeReqs.Load())
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch (len %d want %d)", len(got), len(payload))
	}
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

// TestResume_HashVerifyPassesIntactData pins the per-chunk hash resume path:
// an interrupted download persists ChunkHashes in its control file; on resume,
// intact completed chunks pass the local-disk hash verification, so only the
// missing chunk is fetched and the final file is byte-identical.
func TestResume_HashVerifyPassesIntactData(t *testing.T) {
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

	// Pass 1: chunk 3 permanently fails → control file persists with hashes
	// for chunks 0..2.
	if err := newMgr(failSrv.URL, nil).Run(context.Background(), failSrv.URL, 1); err == nil {
		t.Fatalf("pass 1 should fail on the always-failing chunk")
	}
	path := filepath.Join(dir, "out.bin")
	cf, err := storage.LoadControl(path)
	if err != nil {
		t.Fatalf("load control: %v", err)
	}
	if len(cf.ChunkHashes) != 3 {
		t.Fatalf("expected 3 recorded chunk hashes after the interrupted pass, got %d (map=%v)",
			len(cf.ChunkHashes), cf.ChunkHashes)
	}
	if len(cf.Completed) != 3 {
		t.Fatalf("Completed = %v, want 3 entries", cf.Completed)
	}

	// Pass 2: intact data must pass the hash check and resume (no re-download).
	logf, msgs, mu := captureLog()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := newMgr(goodSrv.URL, logf).Run(ctx, goodSrv.URL, 1); err != nil {
		t.Fatalf("pass 2 resume should succeed: %v", err)
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
		t.Fatalf("intact data must pass the hash integrity check, logs: %v", *msgs)
	}
}

// TestResume_HashDetectsCorruptChunk pins the per-chunk hash resume check:
// when an interrupted download's control file carries ChunkHashes, a corrupt
// on-disk completed chunk must be caught by hashing the LOCAL bytes (no server
// round-trip needed) and the engine must re-download from scratch.
func TestResume_HashDetectsCorruptChunk(t *testing.T) {
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
	// with per-chunk hashes for chunks 0..2.
	if err := newMgr(failSrv.URL, nil).Run(context.Background(), failSrv.URL, 1); err == nil {
		t.Fatalf("pass 1 should fail on the always-failing chunk")
	}
	path := filepath.Join(dir, "out.bin")
	cf, err := storage.LoadControl(path)
	if err != nil {
		t.Fatalf("load control: %v", err)
	}
	if len(cf.ChunkHashes) != 3 {
		t.Fatalf("expected 3 recorded chunk hashes, got %d (map=%v)", len(cf.ChunkHashes), cf.ChunkHashes)
	}

	// Corrupt one byte inside the completed chunk 1 region (offset 1024-2047).
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[1500] ^= 0xFF
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// Pass 2: resume must detect the corrupt chunk via its recorded hash and
	// re-download everything from scratch.
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

// TestResume_LegacyControlFile_UsesServerCompare pins backward compatibility:
// a hand-written v0.x control file WITHOUT ChunkHashes must still resume
// through the legacy server-side sample compare (not the hash path) and
// complete byte-identically.
func TestResume_LegacyControlFile_UsesServerCompare(t *testing.T) {
	payload := make([]byte, 4*1024) // 4 chunks of 1 KiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeServer(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	// Hand-write a legacy control file claiming chunks 0..2 are done, and put
	// those chunks' (correct) bytes on disk ourselves — exactly what an old
	// interrupted download would have left behind.
	if err := os.WriteFile(path, payload[:3*1024], 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf(`{
  "url": %q,
  "total_size": %d,
  "chunk_size": 1024,
  "completed": [0, 1024, 2048]
}`, srv.URL, len(payload))
	if err := os.WriteFile(path+".odm", []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	logf, msgs, mu := captureLog()
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Run(ctx, srv.URL, 1); err != nil {
		t.Fatalf("legacy resume should succeed: %v", err)
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
		t.Fatalf("expected the resume path to be taken (legacy fallback), logs: %v", *msgs)
	}
	if sawVerify {
		t.Fatalf("intact legacy data must pass the server-compare check, logs: %v", *msgs)
	}
}

// TestResume_HashPathStillDetectsServerDrift pins the M1 regression: a resume
// with full per-chunk hash coverage must STILL run the sampled server-side
// compare. A server that replaced the file with same-size, no-ETag content
// goes unnoticed by the local hashes (they match the ORIGINAL bytes on disk),
// so only the server compare can catch the drift — otherwise the resume would
// stitch old completed chunks to the new tail and report success on a mixed,
// corrupt file. The final file must byte-match the NEW payload exactly.
func TestResume_HashPathStillDetectsServerDrift(t *testing.T) {
	// F1: payload served during the interrupted first pass.
	f1 := make([]byte, 4*1024)
	for i := range f1 {
		f1[i] = byte(i % 251)
	}
	// F2: same size, no ETag, differs from F1 at every byte.
	f2 := make([]byte, len(f1))
	for i := range f2 {
		f2[i] = f1[i] ^ 0xFF
	}
	failSrv := serveFlakyChunk(t, f1, 3*1024, 1<<30) // pass 1: chunk 3 always fails
	defer failSrv.Close()
	goodSrv := serveRangeServer(t, f2) // pass 2: different payload, same size, no ETag
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

	// Pass 1: interrupted with hashes recorded for chunks 0..2 of F1.
	if err := newMgr(failSrv.URL, nil).Run(context.Background(), failSrv.URL, 1); err == nil {
		t.Fatalf("pass 1 should fail on the always-failing chunk")
	}
	path := filepath.Join(dir, "out.bin")
	cf, err := storage.LoadControl(path)
	if err != nil {
		t.Fatalf("load control: %v", err)
	}
	if len(cf.ChunkHashes) != 3 {
		t.Fatalf("expected 3 recorded chunk hashes, got %d (map=%v)", len(cf.ChunkHashes), cf.ChunkHashes)
	}

	// Pass 2: same size, no ETag, different bytes. The local hashes still
	// match (disk holds F1's chunks), so ONLY the server-side compare can
	// catch the drift. It must trigger a full re-download from scratch.
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
	if !bytes.Equal(got, f2) {
		t.Fatalf("final content must byte-match F2 exactly — a mixed old/new file means the server drift went undetected")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range *msgs {
		if strings.Contains(m, "resume integrity check failed") {
			return
		}
	}
	t.Fatalf("expected a resume integrity warning (server drift), logs: %v", *msgs)
}

// TestResume_PartialHashCoverageFallsBackToServerCompare pins the S1 fix: a
// control file whose ChunkHashes cover only SOME completed chunks (e.g. a
// hash-less v1.x file that was resumed once — new chunks got hashes, legacy
// completed chunks didn't — then interrupted again) must NOT fail the resume.
// With partial coverage the engine must fall back to the legacy server-side
// compare for the whole set; treating the gap as fatal would wipe intact
// legacy progress and re-download from scratch.
func TestResume_PartialHashCoverageFallsBackToServerCompare(t *testing.T) {
	payload := make([]byte, 4*1024) // 4 chunks of 1 KiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeServer(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	// On disk: correct bytes for chunks 0..2 (completed during an earlier run).
	if err := os.WriteFile(path, payload[:3*1024], 0o644); err != nil {
		t.Fatal(err)
	}
	// Control file: chunks 0..2 completed, but only chunk 0 carries a hash.
	hash0 := sha256.Sum256(payload[:1024])
	cf := &storage.ControlFile{
		URL:         srv.URL,
		TotalSize:   int64(len(payload)),
		ChunkSize:   1024,
		Completed:   []int64{0, 1024, 2048},
		ChunkHashes: map[int64]string{0: hex.EncodeToString(hash0[:])},
	}
	if err := storage.SaveControl(path, cf); err != nil {
		t.Fatalf("save control: %v", err)
	}

	logf, msgs, mu := captureLog()
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Run(ctx, srv.URL, 1); err != nil {
		t.Fatalf("partial-coverage resume should succeed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("final content mismatch (len %d want %d)", len(got), len(payload))
	}
	// The resume must NOT have fallen back to a full re-download: the missing
	// hash for chunk 1024 is a coverage gap, not corruption.
	mu.Lock()
	defer mu.Unlock()
	for _, m := range *msgs {
		if strings.Contains(m, "resume integrity check failed") {
			t.Fatalf("partial hash coverage must fall back to the server compare, not fail the resume; logs: %v", *msgs)
		}
	}
}
