package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"odm/internal/download"
)

// itoaX is a tiny strconv-free helper (mirrors the download package's test
// helpers; kept local so this package's tests don't share internals).
func itoaX(n int) string { return fmt.Sprintf("%d", n) }

// serveSlowSized streams the whole payload in fixed pieces with a drip between
// writes, so each task's transfer takes long enough to overlap another task's
// if the scheduler ever wrongly admits two at once. It also tracks the maximum
// number of concurrent requests so a test can assert the slot budget held.
func serveSlowSized(t *testing.T, payload []byte, step int, drip time.Duration) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var cur, max atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := cur.Add(1)
		for {
			m := max.Load()
			if c <= m || max.CompareAndSwap(m, c) {
				break
			}
		}
		defer cur.Add(-1)

		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaX(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		// Range is ignored on purpose: every GET serves the full body slowly.
		w.Header().Set("Content-Length", itoaX(len(payload)))
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			defer f.Flush()
		}
		for i := 0; i < len(payload); i += step {
			end := i + step
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write(payload[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(drip)
		}
	}))
	return srv, &max
}

// liveQueuedCounts is a race-free count of the scheduler's live/queued maps.
// len(LiveViews())/len(QueuedViews()) would work but snapshot each task, and
// Snapshot reads the task's probe result while Start may still be writing it
// (see task.go Snapshot vs Start) — the poll loop below would race.
func liveQueuedCounts(s *Scheduler) (live, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live), len(s.queued)
}

// TestAdmission_NeverExceedsSlotBudget guards the admitNext fix: the free-slot
// check, the queue pop and the live-map insert must happen in one critical
// section. Previously an RPC Enqueue and the Run loop could both pass the
// "live < slots" check before either registered its task, temporarily running
// one extra task. Two tasks added concurrently to a 1-slot scheduler must
// strictly serialize — at most one request in flight at any time.
func TestAdmission_NeverExceedsSlotBudget(t *testing.T) {
	payload := make([]byte, 512)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv, max := serveSlowSized(t, payload, 128, 10*time.Millisecond)
	defer srv.Close()

	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 1,
		ChunkSize:   4096,
		Retry:       0,
		Timeout:     10 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(1, mgr.NewTask, nil)
	d := NewDaemon(sch, mgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.AddURL(srv.URL, 1); err != nil {
				t.Errorf("AddURL: %v", err)
			}
		}()
	}
	wg.Wait()

	// Wait for both tasks to drain through the single slot.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		l, q := liveQueuedCounts(d.sch)
		if l == 0 && q == 0 && len(d.sch.StoppedViews()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(d.sch.StoppedViews()) < 2 {
		l, q := liveQueuedCounts(d.sch)
		t.Fatalf("tasks never drained: live=%d queued=%d stopped=%d",
			l, q, len(d.sch.StoppedViews()))
	}
	if m := max.Load(); m > 1 {
		t.Fatalf("admission overshot the slot budget: %d concurrent requests for 1 slot", m)
	}
}

// TestStoppedRetentionIsBounded pins the tellStopped retention cap: a daemon
// with many finished tasks must not keep every one in memory — the stopped
// list is bounded to maxStoppedTasks, oldest dropped first.
func TestStoppedRetentionIsBounded(t *testing.T) {
	old := maxStoppedTasks
	maxStoppedTasks = 2
	defer func() { maxStoppedTasks = old }()

	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 1,
		ChunkSize:   4096,
		Retry:       0,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(1, mgr.NewTask, nil)
	d := NewDaemon(sch, mgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	for range 4 {
		if _, err := d.AddURL("http://example.invalid/x", 1); err != nil {
			t.Fatalf("AddURL: %v", err)
		}
	}

	// All four fail fast on the unresolvable URL; once the queue has drained,
	// the stopped list must be exactly the cap (2), not 4.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l, q := liveQueuedCounts(d.sch)
		if l == 0 && q == 0 && len(d.sch.StoppedViews()) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, q := liveQueuedCounts(d.sch)
	t.Fatalf("stopped list not bounded: live=%d queued=%d stopped=%d",
		l, q, len(d.sch.StoppedViews()))
}

// TestEnqueue_RejectsAfterShutdown verifies that Enqueue returns an error when
// the scheduler is winding down, instead of silently dropping the task. The RPC
// layer must propagate this error so the client does not receive a fake task ID.
func TestEnqueue_RejectsAfterShutdown(t *testing.T) {
	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 1,
		ChunkSize:   4096,
		Retry:       0,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(1, mgr.NewTask, nil)
	d := NewDaemon(sch, mgr)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	// Cancel the context to trigger winding-down.
	cancel()

	// Wait for the scheduler to observe the cancellation.
	deadline := time.Now().Add(3 * time.Second)
	for !d.Dead() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Enqueue after shutdown must return an error.
	tk, _, _ := mgr.NewTask("http://example.invalid/x", -1)
	err = sch.Enqueue(&scheduledTask{task: tk, conns: 1}, ctx)
	if err == nil {
		t.Fatal("Enqueue after shutdown must return an error")
	}
}

// TestEnqueue_RejectsWhenQueueFull verifies that Enqueue returns an error when
// the pending queue has reached maxPendingTasks, providing backpressure against
// unlimited RPC addUri calls.
func TestEnqueue_RejectsWhenQueueFull(t *testing.T) {
	// Use a 0-slot scheduler so no task is ever admitted from the queue — every
	// Enqueue stays queued, allowing us to fill the queue to the limit without
	// tasks draining out.
	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 1,
		ChunkSize:   4096,
		Retry:       0,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(0, mgr.NewTask, nil)
	ctx := context.Background()

	// Fill the queue to the limit. Each Enqueue must succeed until the limit.
	filled := 0
	for i := 0; i < maxPendingTasks; i++ {
		tk, _, _ := mgr.NewTask("http://example.invalid/fill", -1)
		if err := sch.Enqueue(&scheduledTask{task: tk, conns: 1}, ctx); err != nil {
			t.Fatalf("Enqueue at %d: %v", i, err)
		}
		filled++
	}
	if filled != maxPendingTasks {
		t.Fatalf("expected to fill %d slots, filled %d", maxPendingTasks, filled)
	}

	// The next Enqueue must fail with "too many pending tasks".
	tk, _, _ := mgr.NewTask("http://example.invalid/overflow", -1)
	if err := sch.Enqueue(&scheduledTask{task: tk, conns: 1}, ctx); err == nil {
		t.Fatal("Enqueue beyond pending limit must fail")
	}
}

// TestEnqueue_ConcurrentEnqueue proves the pending-task bound holds under
// concurrent enqueue pressure. Multiple goroutines race to fill the queue;
// after all finish, len(queued) must not exceed maxPendingTasks and no
// bookkeeping invariant is violated.
func TestEnqueue_ConcurrentEnqueue(t *testing.T) {
	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 1,
		ChunkSize:   4096,
		Retry:       0,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(0, mgr.NewTask, nil)
	ctx := context.Background()

	const goroutines = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCount := 0
	var errMu sync.Mutex

	for range goroutines {
		go func() {
			defer wg.Done()
			tk, _, _ := mgr.NewTask("http://example.invalid/concurrent", -1)
			if err := sch.Enqueue(&scheduledTask{task: tk, conns: 1}, ctx); err != nil {
				errMu.Lock()
				errCount++
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Some enqueues must have been rejected (200 > 128).
	if errCount == 0 {
		t.Fatal("expected some concurrent enqueues to be rejected")
	}

	// Queue must not exceed the limit.
	sch.mu.Lock()
	qlen := len(sch.queued)
	sch.mu.Unlock()
	if qlen > maxPendingTasks {
		t.Fatalf("queue length %d exceeds max %d", qlen, maxPendingTasks)
	}
}

// TestEnqueue_ShutdownRace proves there is no race between concurrent AddURL
// calls and scheduler shutdown. This is the exact scenario from the audit
// finding: RPC Enqueue racing with scheduler cancellation / Wait.
func TestEnqueue_ShutdownRace(t *testing.T) {
	for range 50 {
		mgr, err := download.NewManager(download.ExecOptions{
			Dir:         t.TempDir(),
			Connections: 1,
			ChunkSize:   4096,
			Retry:       0,
			Timeout:     5 * time.Second,
			CheckCert:   true,
		}, nil)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		sch := NewEmptyScheduler(1, mgr.NewTask, nil)
		d := NewDaemon(sch, mgr)
		ctx, cancel := context.WithCancel(context.Background())
		d.Start(ctx)

		var wg sync.WaitGroup

		// Goroutine A: repeatedly AddURL until shutdown.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = d.AddURL("http://example.invalid/race", 1)
			}
		}()

		// Goroutine B: cancel after a short delay.
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Microsecond)
			cancel()
		}()

		wg.Wait()

		// Wait for daemon to be dead.
		deadline := time.Now().Add(3 * time.Second)
		for !d.Dead() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
}
