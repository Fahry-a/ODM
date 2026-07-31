package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// serveSlowRangeExact drips a ranged resource in fixed-size pieces with a delay
// between writes, like serveSlowRange — but sets the response Content-Length to
// the requested RANGE size (serveSlowRange sets it to the whole-file size, which
// is fine for the whole-file range its progress test uses but aborts partial
// ranges with an unexpected EOF). Used by the pause test to keep N workers
// mid-chunk for a controllable window.
func serveSlowRangeExact(t *testing.T, payload []byte, step int, drip time.Duration) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Length", itoaS(int(end-start+1)))
		w.WriteHeader(http.StatusPartialContent)
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

// TestPause_UnpauseWakesAllWorkers is the regression test for the pause
// broadcast bug. Unpause used to send ONE non-blocking token on a buffered(1)
// channel, which woke only a single worker blocked in the pause gate and left
// the other N-1 blocked forever — so Start's workerWg.Wait() never returned
// and the task hung even though it had been unpaused. The fix CLOSES the
// channel (a broadcast: every blocked worker wakes) and installs a fresh
// channel for the next pause cycle.
//
// The test runs a real multi-worker download against a slow-drip server,
// pauses once all workers are live, lets them rendezvous in the pause gate,
// unpauses, and demands the task reach a terminal state within a deadline.
// The timing is chosen so the failure is deterministic against the old
// one-shot-send code:
//   - 128 chunks at ~80ms each: 4 workers finish in ~2.6s, but ONE surviving
//     worker alone needs ~10s to drain the queue (the other 3 stay parked in
//     the gate forever, so Start hangs well past the 5s assertion deadline;
//     the 12s context is long enough that it cannot "rescue" the hang before
//     the assertion fires).
func TestPause_UnpauseWakesAllWorkers(t *testing.T) {
	const workers = 4
	payload := make([]byte, 2*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	// 16 KiB dripped every 80ms per request → ~80ms per 16 KiB chunk, so
	// there is a wide window where all 4 workers are live, and after Pause
	// they all finish their in-flight chunk and park in the pause gate.
	srv := serveSlowRangeExact(t, payload, 16*1024, 80*time.Millisecond)
	defer srv.Close()

	dir := t.TempDir()
	m, err := NewManager(ExecOptions{
		Dir:         dir,
		OutFile:     "pause.bin",
		Connections: workers,
		Retry:       0,
		RetryWait:   5 * time.Millisecond,
		Continue:    false,
		ChunkSize:   16 * 1024,
		Timeout:     30 * time.Second,
		MaxRedirect: 5,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	tk, _, derr := m.NewTask(srv.URL, 0)
	if derr != nil {
		t.Fatalf("NewTask: %v", derr)
	}

	// 12s context vs 5s assertion deadline: even if the old bug left workers
	// stuck, the context must NOT fire (and return "success" once the single
	// surviving worker drains the queue) before the deadline trips the test.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tk.Start(ctx, workers, nil)
	}()

	// Wait until all workers are live (mid-download). Start probes first, so
	// Connections is 0 until workers actually launch. Poll the atomic counter
	// directly (not Snapshot, which also reads the probe result and would race
	// with Start's in-flight probe).
	deadline := time.Now().Add(10 * time.Second)
	for tk.Connections() != workers {
		if time.Now().After(deadline) {
			t.Fatalf("workers never reached %d (conns=%d, state=%s)",
				workers, tk.Connections(), tk.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Pause, then give every worker time to finish its in-flight chunk and
	// rendezvous in the pause gate (a chunk takes ~80ms).
	tk.Pause()
	time.Sleep(200 * time.Millisecond)

	// Unpause must wake ALL of them, not just one.
	tk.Unpause()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("download failed after unpause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("task did not reach a terminal state after unpause — " +
			"workers stuck in the pause gate (regression: one-shot wake-up)")
	}

	got, err := os.ReadFile(filepath.Join(dir, "pause.bin"))
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("size: want %d got %d", len(payload), len(got))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], payload[i])
		}
	}
}
