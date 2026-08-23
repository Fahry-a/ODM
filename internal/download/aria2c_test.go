package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
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

// ---------------------------------------------------------------------------
// AriaSplit — the segment math (aria2c --split / --min-split-size).
// ---------------------------------------------------------------------------

func TestAriaSplit(t *testing.T) {
	cases := []struct {
		size, split, min int64
		wantN, wantSeg   int64
	}{
		{100, 4, 1, 4, 25},                 // 4 segments of 25
		{100, 4, 30, 1, 100},               // 2*30 > 100 → no split
		{100, 4, 20, 2, 50},                // 2*20 ≤ 100 → 2 segments
		{100, 4, 10, 4, 25},                // plenty of room for all 4
		{100, 1, 1, 1, 100},                // split 1 → single
		{100, 10, 1, 10, 10},               // more split than segments fit → 10
		{0, 4, 1, 1, 0},                    // sizeless
		{100, 0, 1, 1, 100},                // invalid split → single
		{100, 4, 0, 4, 25},                 // invalid min → no guard
		{1 << 30, 8, 20 << 20, 8, 1 << 27}, // 1 GiB, min 20M → 8 segments of 128 MiB
	}
	for _, tc := range cases {
		n, seg := AriaSplit(tc.size, tc.split, tc.min)
		if n != tc.wantN || seg != tc.wantSeg {
			t.Errorf("AriaSplit(%d,%d,%d) = (%d,%d), want (%d,%d)",
				tc.size, tc.split, tc.min, n, seg, tc.wantN, tc.wantSeg)
		}
	}
}

// ---------------------------------------------------------------------------
// StaticQueue — one segment per worker, deterministic resume layout.
// ---------------------------------------------------------------------------

func TestStaticQueue_CoverageAndNoStealing(t *testing.T) {
	q := NewStaticQueue(100, 4)
	total := int64(0)
	segs := 0
	for {
		c, ok := q.Next()
		if !ok {
			break
		}
		segs++
		total += c.End - c.Start + 1
		if c.Start == 0 && c.Index == 0 && (c.End-c.Start+1) != 25 {
			t.Fatalf("first segment must be 25 bytes, got %d", c.End-c.Start+1)
		}
	}
	if segs != 4 {
		t.Fatalf("want 4 segments, got %d", segs)
	}
	if total != 100 {
		t.Fatalf("segments must cover exactly 100 bytes, got %d", total)
	}
	// Next after drain → false (no work-stealing for more workers).
	if _, ok := q.Next(); ok {
		t.Fatal("drained queue must return false")
	}
	// Requeue returns false: a segment whose retry budget is exhausted fails
	// the task (error accounting) instead of being silently skipped — the old
	// no-op-true produced "completed" files with un-downloaded holes.
	if q.Requeue(Chunk{}, 0) {
		t.Fatal("Requeue must return false so exhausted segments fail the task")
	}
}

func TestStaticQueue_ResumeRoundTrip(t *testing.T) {
	q := NewStaticQueue(100, 4)
	// Claim + complete segments 0 and 2 (offsets 0 and 50).
	seen := map[int64]struct{}{}
	for {
		c, ok := q.Next()
		if !ok {
			break
		}
		if c.Start == 0 || c.Start == 50 {
			q.MarkDone(c)
			seen[c.Start] = struct{}{}
		}
	}
	offs := q.CompletedOffsets()
	if len(offs) != 2 || offs[0] != 0 || offs[1] != 50 {
		t.Fatalf("CompletedOffsets = %v, want [0 50]", offs)
	}

	// Rebuild the same layout and pre-seed from the offsets.
	r := NewStaticQueue(100, 4)
	done := map[int64]struct{}{}
	for _, off := range offs {
		done[off] = struct{}{}
	}
	already, ok := r.ResetCompletedOffsets(done, 100)
	if !ok || already != 50 {
		t.Fatalf("ResetCompletedOffsets = (%d,%v), want (50,true)", already, ok)
	}
	// The remaining segments are exactly the un-done ones.
	got := []int64{}
	for {
		c, okk := r.Next()
		if !okk {
			break
		}
		got = append(got, c.Start)
	}
	if len(got) != 2 || got[0] != 25 || got[1] != 75 {
		t.Fatalf("remaining segments = %v, want [25 75]", got)
	}
}

// ---------------------------------------------------------------------------
// aria2c profile end-to-end over HTTP/2.
// ---------------------------------------------------------------------------

func TestAria2cProfile_UsesHTTP2SingleConn(t *testing.T) {
	payload := make([]byte, 10*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var accepts atomic.Int32
	var mu sync.Mutex
	protoCount := map[string]int{}
	streams := map[string]struct{}{}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protoCount[r.Proto]++
		if r.ProtoMajor == 2 {
			streams[r.Header.Get("x-odm-stream")] = struct{}{}
		}
		mu.Unlock()
		http.ServeContent(w, r, "file.bin", time.Time{}, strings.NewReader(string(payload)))
	}))
	srv.EnableHTTP2 = true
	// Count distinct TCP connections via ConnContext (fires once per accepted
	// connection) — avoids touching the listener, which StartTLS needs intact
	// to install its TLS wrapper.
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		accepts.Add(1)
		return ctx
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second, HTTP2: true, CheckCertificate: false})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-h2"), srv.URL, TaskOptions{
		OutputName:       "out.bin",
		Dir:              dir,
		Retry:            2,
		Timeout:          10 * time.Second,
		ChunkSize:        1 * 1024 * 1024, // min chunk
		MinSplitSize:     1 * 1024 * 1024,
		Split:            4,
		Profile:          "aria2c",
		MaxConnPerServer: 4,
	}, cli, lim, nil)

	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("aria2c h2 download failed: %v", err)
	}

	// The whole 10 MiB must be byte-identical.
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) || string(got) != string(payload) {
		t.Fatalf("file mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	// HTTP/2 negotiation: every request saw ProtoMajor==2.
	if protoCount["HTTP/2.0"] == 0 || protoCount["HTTP/1.1"] != 0 {
		t.Fatalf("expected all h2 requests, got %v", protoCount)
	}
	// Exactly ONE TCP connection for all 4 segments (h2 multiplexing).
	if n := accepts.Load(); n != 1 {
		t.Fatalf("expected exactly 1 TCP accept (h2 multiplexing), got %d", n)
	}
	// All requests were h2 streams over that one connection.
	if len(streams) < 4 {
		// streams map is populated by x-odm-stream header (unset), so this is
		// informational; the real assertions are proto==h2 and 1 TCP conn.
		t.Logf("note: saw %d distinct h2 stream ids", len(streams))
	}
}

// TestAria2cProfile_ResumeKeepsProgress pins the aria2c-profile resume path:
// completed segments are LARGE static splits (AriaSplit), not the 4 MiB-ish
// opts.ChunkSize the resume hash verifier used to assume. Hashing a segment
// with the wrong span (a prefix of the real one) made the digest never match,
// so every interrupted aria2c download was discarded as a failed integrity
// check and re-downloaded from scratch. The crafted control file below has 3
// of 4 segments complete; the resume must pass verification and reuse them.
func TestAria2cProfile_ResumeKeepsProgress(t *testing.T) {
	payload := make([]byte, 16*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeSlices(payload)
	defer srv.Close()
	dir := t.TempDir()

	// Layout: AriaSplit(16M, split=4, minSplit=1M) → 4 segments of 4 MiB.
	segSize := int64(4 * 1024 * 1024)
	completed := []int64{}
	hashes := map[int64]string{}
	for off := int64(0); off < 3*segSize; off += segSize {
		sum := sha256.Sum256(payload[off : off+segSize])
		hashes[off] = hex.EncodeToString(sum[:])
		completed = append(completed, off)
	}
	// Segments 0-2 are on disk; segment 3 (offset 12 MiB) is the missing one.
	if err := os.WriteFile(filepath.Join(dir, "out.bin"), payload[:12*1024*1024], 0o644); err != nil {
		t.Fatal(err)
	}
	cf := &storage.ControlFile{
		URL:         srv.URL,
		FinalURL:    srv.URL,
		TotalSize:   int64(len(payload)),
		ChunkSize:   segSize,
		Completed:   completed,
		ChunkHashes: hashes,
		Profile:     "aria2c",
	}
	if err := storage.SaveControl(filepath.Join(dir, "out.bin"), cf); err != nil {
		t.Fatal(err)
	}

	// Pass 2: resume with the same profile but a ChunkSize that deliberately
	// differs from the segment size — the bug used opts.ChunkSize for the span.
	logf, msgs, mu := captureLog()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second, HTTP2: true, CheckCertificate: false})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-aria-resume"), srv.URL, TaskOptions{
		OutputName:       "out.bin",
		Dir:              dir,
		Retry:            2,
		Timeout:          10 * time.Second,
		ChunkSize:        1 * 1024 * 1024,
		MinSplitSize:     1 * 1024 * 1024,
		Split:            4,
		Profile:          "aria2c",
		MaxConnPerServer: 4,
		Continue:         true,
	}, cli, lim, logf)
	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("aria2c resume failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed aria2c file mismatch (len %d want %d)", len(got), len(payload))
	}
	mu.Lock()
	defer mu.Unlock()
	var sawResume bool
	for _, m := range *msgs {
		if strings.Contains(m, "resume integrity check failed") {
			t.Fatalf("intact segments must pass the integrity check, logs: %v", *msgs)
		}
		if strings.Contains(m, "bytes already written") {
			sawResume = true
		}
	}
	if !sawResume {
		t.Fatalf("expected the resume path to be taken, logs: %v", *msgs)
	}
}

// TestAria2cProfile_DegradesToH1: an h1-only server must still download fine
// under the aria2c profile (Go falls back to HTTP/1.1 — no hard failure).
func TestAria2cProfile_DegradesToH1(t *testing.T) {
	payload := []byte("degraded-server-content-that-is-not-range-aware")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoaS(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second, HTTP2: true})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-degrade"), srv.URL, TaskOptions{
		OutputName:       "out.bin",
		Dir:              dir,
		Retry:            2,
		Timeout:          10 * time.Second,
		ChunkSize:        1 * 1024 * 1024,
		MinSplitSize:     1 * 1024 * 1024,
		Split:            4,
		Profile:          "aria2c",
		MaxConnPerServer: 1,
	}, cli, lim, nil)
	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("h1 degradation failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("h1 degradation produced wrong file: %v", err)
	}
}

// TestAria_ConnsDisplayCappedToSplit pins the [xN] fix: with 4 segments and a
// 16-connection budget, Snapshot().Connections must report 4 (the actual
// worker count), not the raw budget.
func TestAria_ConnsDisplayCappedToSplit(t *testing.T) {
	payload := make([]byte, 200*1024*1024) // big enough: split_eff = 4 at minSplit 20M
	srv := serveRangeServer(t, payload)
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      1,
		RetryWait:  time.Millisecond,
		Timeout:    10 * time.Second,
		Profile:    "aria2c",
		Split:      4, // explicit --split 4 → ariaSplit must be ≤ 4
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("aria2c")
	var maxSeen atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c := int32(task.Snapshot().Connections); c > 0 {
				for {
					cur := maxSeen.Load()
					if c <= cur || maxSeen.CompareAndSwap(cur, c) {
						break
					}
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := task.Start(context.Background(), 16, nil); err != nil {
		t.Fatal(err)
	}
	close(stop)
	if got := maxSeen.Load(); got > 4 {
		t.Fatalf("max Snapshot Connections during run = %d, want ≤ split (4)", got)
	}
}

// TestAria_ExhaustedSegmentFailsTask pins the C1 fix: when downloadChunk's
// retry budget is exhausted on a static segment, the task must FAIL (control
// file kept for resume) — the old Requeue-always-true silently skipped the
// segment and reported "completed" with an un-downloaded hole.
func TestAria_ExhaustedSegmentFailsTask(t *testing.T) {
	payload := make([]byte, 4*1024*1024)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	// Segment starting at 2 MiB always fails with a transient (500) status.
	failStart := int64(2 * 1024 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		start, _, _ := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if start == failStart {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s, e, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(s))+"-"+itoaS(int(e))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[s : e+1])
	}))
	defer srv.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t"), srv.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      1, // small budget so exhaustion happens fast
		RetryWait:  time.Millisecond,
		Timeout:    10 * time.Second,
		ChunkSize:  1024 * 1024,
		Profile:    "aria2c",
		Split:      4,
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("aria2c")
	err = task.Start(context.Background(), 2, nil)
	if err == nil {
		t.Fatal("task must FAIL when a static segment exhausts its retries")
	}
	if task.State() != StateError {
		t.Fatalf("state = %v, want error", task.State())
	}
	// Control file survives for --continue.
	if _, cerr := os.Stat(filepath.Join(dir, "out.bin.odm")); cerr != nil {
		t.Fatalf("control file must survive a failed aria task for resume: %v", cerr)
	}
}
