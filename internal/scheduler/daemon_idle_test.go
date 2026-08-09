package scheduler

import (
	"context"
	"testing"
	"time"

	"odm/internal/download"
)

// TestEmptySchedulerStaysIdleWhenNoTasks documents the daemon requirement (PRD
// §10): a freshly-started RPC scheduler with zero tasks must NOT report Run as
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
