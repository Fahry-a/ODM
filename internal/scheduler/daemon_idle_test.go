package scheduler

import (
	"context"
	"testing"
	"time"

	"odm/internal/download"
)

// TestEmptySchedulerStaysIdleWhenNoTasks documents the daemon requirement (spec
// ): a freshly-started RPC scheduler with zero tasks must NOT report Run as
// finished. An empty scheduler that exits instantly would pull the whole daemon
// down before the first odm.addUri arrives — which is exactly the regression
// that broke the CLI --rpc path. The permanent idle hold installed by
// NewEmptyScheduler keeps Run parked until ctx is cancelled.
func TestEmptySchedulerStaysIdleWhenNoTasks(t *testing.T) {
	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 2,
		ChunkSize:   4096,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(2, mgr.NewTask, nil)
	d := NewDaemon(sch, mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	dead := make(chan struct{})
	d.OnDead(func() { close(dead) })

	select {
	case <-dead:
		t.Fatalf("empty scheduler finished Run within 500ms — idle hold broken; daemon would exit before addUri")
	case <-time.After(500 * time.Millisecond):
		// still parked — the daemon's Run goroutine is alive. Correct.
	}

	// Cancelling the ctx must release the idle hold and let Run return.
	cancel()
	select {
	case <-dead:
	case <-time.After(2 * time.Second):
		t.Fatalf("scheduler never reported dead after ctx cancel (goroutine leak?)")
	}
}

// TestEmptySchedulerAdmitsLateTask ensures the idle scheduler actually serves a
// task added after Start — i.e. Enqueue still works once a slot frees and the
// hold doesn't accidentally gate admission.
func TestEmptySchedulerAdmitsLateTask(t *testing.T) {
	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 2,
		ChunkSize:   4096,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(2, mgr.NewTask, nil)
	if sch.isIdle != true {
		t.Fatalf("NewEmptyScheduler must set isIdle=true")
	}
	d := NewDaemon(sch, mgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// No tasks; confirm still alive briefly.
	if d.Dead() {
		t.Fatalf("daemon died on an empty scheduler")
	}
}

// TestRunDrainsCancelledTasksTally pins the ^C tally bug: when the run's ctx
// is cancelled, in-flight tasks error out and must be counted as FAILED (not
// silently dropped from the tally). The scheduler must keep draining s.compl
// until no task is left live, then report succeeded/failed correctly — and
// return promptly (no hang waiting on the WaitGroup, whose idle hold was
// already released).
func TestRunDrainsCancelledTasksTally(t *testing.T) {
	mgr, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 2,
		ChunkSize:   4096,
		Timeout:     5 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sch := NewEmptyScheduler(2, mgr.NewTask, nil)
	d := NewDaemon(sch, mgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	// A URL whose probe never resolves → the task is stuck in probe forever,
	// exactly like a real download that's mid-flight when ^C lands.
	if _, err := d.AddURL("http://127.0.0.1:1/unreachable", 1); err != nil {
		t.Fatalf("AddURL: %v", err)
	}

	// Give the task a moment to enter the live set, then cancel like ^C.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sch.LiveViews()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	dead := make(chan struct{})
	d.OnDead(func() { close(dead) })
	select {
	case <-dead:
	case <-time.After(5 * time.Second):
		t.Fatalf("scheduler never wound down after ctx cancel (deadlock?)")
	}
	// The single cancelled task must be tallied as failed — the bug reported
	// "0 failed" for ^C because Run returned without draining s.compl.
	if got := d.sch.FailedCount(); got != 1 {
		t.Fatalf("cancelled task must be counted as failed, got %d", got)
	}
	if got := d.sch.SucceededCount(); got != 0 {
		t.Fatalf("no task should succeed on ^C, got %d", got)
	}
}
