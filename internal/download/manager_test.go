package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"odm/internal/transport"
)

// serveRangeServer is a small range-supporting payload server (mirrors the one
// in transport_test but kept independent here so the download tests don't share
// helper internals with another package).
func serveRangeServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		start, end, ok := parseClientRangeS(rng, len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1]) // server sets Content-Length automatically (fixed buf)
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

func itoaS(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func parseClientRangeS(h string, total int) (start, end int64, ok bool) {
	_, rng, found := cutS(h, "=")
	if !found {
		return 0, 0, false
	}
	sstr, estr, found := cutS(rng, "-")
	if !found {
		return 0, 0, false
	}
	s := int64(atoiS(sstr))
	e := int64(atoiS(estr))
	if e < 0 || e >= int64(total) {
		e = int64(total) - 1
	}
	return s, e, true
}

func cutS(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func atoiS(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// TestManager_SingleFile downloads a deterministic payload with 4 connections
// and asserts the file content + size matches.
func TestManager_SingleFile(t *testing.T) {
	payload := make([]byte, 1024*256)
	for i := range payload {
		payload[i] = byte(i)
	}
	srv := serveRangeServer(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	wantSHA := sha256.Sum256(payload)
	m, err := NewManager(ExecOptions{
		Dir:         dir,
		OutFile:     "out.bin",
		Connections: 4,
		Retry:       1,
		RetryWait:   10 * time.Millisecond,
		Continue:    false,
		ChunkSize:   16 * 1024,
		Timeout:     10 * time.Second,
		MaxRedirect: 5,
		CheckCert:   true,
		UserAgent:   "odm-test",
		Checksum:    "sha256:" + hex.EncodeToString(wantSHA[:]),
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Run(ctx, srv.URL, 4); err != nil {
		t.Fatalf("download: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("size: want %d got %d", len(payload), len(got))
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch (first diff bytes)")
	}

	// Checksum verify via the package-level helper (same path/algo/expectHex
	// the Manager's tasks use on completion).
	if err := verifyChecksum(filepath.Join(dir, "out.bin"), "sha256", hex.EncodeToString(wantSHA[:])); err != nil {
		t.Fatalf("checksum: %v", err)
	}
}

// TestManager_ResumeInterrupted starts a download, cancels mid-flight, then
// reruns with --continue and expects it to finish from where it left off
// without corruption. We assert the final bytes == original payload.
func TestManager_ResumeInterrupted(t *testing.T) {
	payload := make([]byte, 1024*256)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeServer(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	build := func() *Manager {
		t.Helper()
		m, err := NewManager(ExecOptions{
			Dir:         dir,
			OutFile:     "resume.bin",
			Connections: 4,
			Retry:       2,
			RetryWait:   5 * time.Millisecond,
			Continue:    true, // resume enabled for the 2nd pass
			ChunkSize:   8 * 1024,
			Timeout:     5 * time.Second,
			MaxRedirect: 5,
			CheckCert:   true,
		}, nil)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}

	// First pass: cancel quickly so only part of the file lands.
	m1 := build()
	ctx1, cancel1 := context.WithCancel(context.Background())
	_ = m1.Run(ctx1, srv.URL, 4)
	cancel1()

	// Second pass with Continue=true — should fill in the rest.
	m2 := build()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if err := m2.Run(ctx2, srv.URL, 4); err != nil {
		t.Fatalf("resume download: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "resume.bin"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("resumed file content mismatch (len got %d want %d)", len(got), len(payload))
	}
	// Control file must be deleted after success.
	if _, err := os.Stat(filepath.Join(dir, "resume.bin.odm")); !os.IsNotExist(err) {
		t.Fatalf("control file not removed after successful resume")
	}
}

// ensure transport is referenced so its package compiles in this file's package
// graph (it is used transitively via Manager).
var _ = transport.SkipBody

var _ = io.Discard

// TestDropTerminalTasks_PrunesOldestFirst pins the registry retention policy:
// once over the cap, terminal (completed/error) tasks are dropped lowest id
// first, while live/queued tasks are never touched.
func TestDropTerminalTasks_PrunesOldestFirst(t *testing.T) {
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5e9})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string, s TaskState) *Task {
		tt := NewTask(TaskID(id), "http://example.invalid", TaskOptions{}, cli, nil, nil)
		tt.setState(s)
		return tt
	}
	reg := map[TaskID]*Task{
		"odm-001": mk("odm-001", StateCompleted),
		"odm-002": mk("odm-002", StateError),
		"odm-003": mk("odm-003", StateCompleted),
		"odm-004": mk("odm-004", StateActive),
		"odm-005": mk("odm-005", StateQueued),
	}
	dropTerminalTasks(reg, 2)
	if len(reg) != 3 {
		t.Fatalf("want 3 entries after pruning 2, got %d", len(reg))
	}
	for _, gone := range []string{"odm-001", "odm-002"} {
		if _, ok := reg[TaskID(gone)]; ok {
			t.Fatalf("%s (oldest terminal) should have been pruned", gone)
		}
	}
	for _, kept := range []string{"odm-003", "odm-004", "odm-005"} {
		if _, ok := reg[TaskID(kept)]; !ok {
			t.Fatalf("%s should be retained", kept)
		}
	}
}

// serveSlowRange streams a ranged resource in fixed-size non-overlapping pieces
// with a delay between writes, so one chunk takes several ~100ms progress-tick
// windows to transfer. This is what exposes the "bar frozen at 0/0 during a long
// single chunk" regression: live progress must be fed from noteBytes mid-stream,
// not only at chunk completion. The client always sends a fully-closed Range
// (bytes=0-(N-1)), so we write contiguous slices — never re-issuing a boundary
// byte — and advance the cursor by the bytes actually written each step.
func serveSlowRange(t *testing.T, payload []byte, step int, drip time.Duration) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoaS(len(payload)))
		if r.Method == http.MethodHead {
			return
		}
		start, end, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if !ok {
			start, end = 0, int64(len(payload))-1
		}
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		if f, ok := w.(http.Flusher); ok {
			defer f.Flush()
		}
		for cur := start; cur <= end; {
			n := int64(step)
			if cur+n-1 > end {
				n = end - cur + 1
			}
			_, _ = w.Write(payload[cur : cur+n])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(drip)
			cur += n
		}
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

// TestProgress_SinkFiresMidStream is the regression test for the frozen-bar
// bug: a download whose single chunk takes several ~100ms progress ticks to
// transfer must surface intermediate progress to the sink during the stream,
// not jump 0→100% once at the end. Before the fix, noteBytes discarded its
// sink and the only snapshot came from worker's per-chunk nudge after MarkDone
// — so the bar stayed at "0/0 completed" for the entire duration of each chunk
// (visible on multi-GiB single-file downloads where one chunk can take seconds
// to arrive).
func TestProgress_SinkFiresMidStream(t *testing.T) {
	// 1 MiB payload in one 4 MiB chunk, dripped in 64 KiB pieces every 25ms →
	// ~16 pieces × 25ms ≈ 400ms stream → comfortably several 100ms progress
	// ticks. The sink must fire mid-stream with strictly-intermediate bytesDone.
	const (
		payloadSize = 1024 * 1024
		step        = 64 * 1024
		drip        = 25 * time.Millisecond
	)
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveSlowRange(t, payload, step, drip)
	defer srv.Close()

	dir := t.TempDir()
	m, err := NewManager(ExecOptions{
		Dir:         dir,
		OutFile:     "prog.bin",
		Connections: 1, // one worker/single chunk → exercises the during-stream path only
		Retry:       1,
		RetryWait:   5 * time.Millisecond,
		Continue:    false,
		ChunkSize:   4 * 1024 * 1024, // > payload ⇒ one chunk
		Timeout:     30 * time.Second,
		MaxRedirect: 5,
		CheckCert:   false,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	tk, _, derr := m.NewTask(srv.URL, 0)
	if derr != nil {
		t.Fatalf("NewTask: %v", derr)
	}

	var (
		calls      int
		maxBytes   int64
		sawPartial bool
		mu         sync.Mutex
	)
	sink := func(v ProgressView) {
		mu.Lock()
		calls++
		if v.BytesDone > maxBytes {
			maxBytes = v.BytesDone
		}
		if v.BytesDone > 0 && v.BytesDone < int64(payloadSize) {
			sawPartial = true
		}
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := tk.Start(ctx, 1, sink); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	gotCalls, gotMax, gotPartial := calls, maxBytes, sawPartial
	mu.Unlock()

	if gotCalls < 2 {
		t.Fatalf("progress sink fired only %d time(s) — bar would be frozen during the stream "+
			"(regression: noteBytes must throttle the sink mid-stream). maxBytes=%d/%d",
			gotCalls, gotMax, payloadSize)
	}
	if !gotPartial {
		t.Fatalf("sink never saw partial progress — bar jumps 0%%→100%% with no animation "+
			"(calls=%d maxBytes=%d/%d)", gotCalls, gotMax, payloadSize)
	}
	// Sanity: the final sample is the whole payload (the per-chunk worker nudge
	// fires with the completed state).
	if gotMax != int64(payloadSize) {
		t.Fatalf("download didn't complete: maxBytes=%d want %d", gotMax, payloadSize)
	}
}
