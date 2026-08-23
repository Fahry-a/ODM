package scheduler

import (
	"context"
	"sync"

	"odm/internal/download"
)

// Daemon wraps a Scheduler for the RPC server: it runs the Scheduler in a
// background goroutine and exposes live mutation (AddURL, Pause, Unpause,
// Remove, list views) so the JSON-RPC methods in internal/rpc can steer
// downloads at runtime. The one-shot CLI path drives the Scheduler
// directly; only the RPC path needs the Daemon.
type Daemon struct {
	sch *Scheduler
	mgr *download.Manager

	mu      sync.Mutex
	started bool
	ctx     context.Context // derived from Start's ctx, cancelled on stop

	stop   context.CancelFunc
	done   chan struct{}
	ondead []func() // called once the scheduler has wound down (shutdown hooks)
}

// NewDaemon builds a Daemon over an already-constructed Scheduler + Manager.
// The Scheduler runs in the background once Start is called.
func NewDaemon(sch *Scheduler, mgr *download.Manager) *Daemon {
	return &Daemon{sch: sch, mgr: mgr}
}

// Start runs the underlying scheduler in the background and returns. The
// Daemon must be started exactly once; subsequent calls are no-ops.
func (d *Daemon) Start(ctx context.Context) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	cctx, cancel := context.WithCancel(ctx)
	d.stop = cancel
	d.done = make(chan struct{})
	d.ctx = cctx
	d.mu.Unlock()

	go func() {
		_, _, _ = d.sch.Run(cctx)
		close(d.done)
		d.mu.Lock()
		hooks := d.ondead
		d.ondead = nil
		d.mu.Unlock()
		for _, h := range hooks {
			h()
		}
	}()
}

// OnDead registers a callback fired exactly once when the background scheduler
// run has wound down (Stop was called, the context was cancelled, or — for the
// one-shot scheduler — all tasks finished). Used by the RPC server's
// odm.shutdown handler so the CLI main can observe the daemon ending and exit
// the process (see rpc.dispatch's odm.shutdown branch).
//
// Crucially, "dead" means the run goroutine completed, NOT merely that the done
// channel exists — d.done is allocated at Start, so a nil check would fire the
// hook immediately on every registration and shut the daemon down right after
// it booted. We test whether the channel is closed instead.
func (d *Daemon) OnDead(h func()) {
	d.mu.Lock()
	closed := d.doneClosed()
	if !closed {
		d.ondead = append(d.ondead, h)
	}
	d.mu.Unlock()
	if closed {
		h()
	}
}

// Dead reports whether the scheduler's background run has finished (the done
// channel is closed). Allocation of d.done at Start must NOT count as dead.
func (d *Daemon) Dead() bool {
	d.mu.Lock()
	if !d.started || d.done == nil {
		d.mu.Unlock()
		return false
	}
	select {
	case <-d.done:
		d.mu.Unlock()
		return true
	default:
		d.mu.Unlock()
		return false
	}
}

// doneClosed reports whether the done channel is already closed. Caller holds
// d.mu; the non-blocking receive is safe under the lock because a closed
// channel always yields immediately without blocking.
func (d *Daemon) doneClosed() bool {
	if !d.started || d.done == nil {
		return false
	}
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// Stop signals the background scheduler to wind down and waits for it.
func (d *Daemon) Stop() {
	d.mu.Lock()
	if d.stop != nil {
		d.stop()
	}
	done := d.done
	d.mu.Unlock()
	if done != nil {
		<-done
	}
}

// AddURL creates a task for url with the given per-file connection budget and
// feeds it into the scheduler's admission path. If no slot is free immediately
// it waits in the queued set like any other batch task.
func (d *Daemon) AddURL(url string, conns int) (download.TaskID, error) {
	t, defaultConns, err := d.mgr.NewTask(url, -1)
	if err != nil {
		return "", err
	}
	if conns <= 0 {
		conns = defaultConns
	}
	st := &scheduledTask{task: t, conns: conns}

	d.mu.Lock()
	ctx := d.ctx
	d.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	// Hand the new task to the scheduler's queue so the next free slot admits it.
	d.sch.Enqueue(st, ctx)
	return t.ID(), nil
}

// Pause marks a task paused by id (no-op if unknown).
func (d *Daemon) Pause(id download.TaskID) bool {
	t := d.mgr.Task(id)
	if t == nil {
		return false
	}
	t.Pause()
	return true
}

// Unpause resumes a paused task.
func (d *Daemon) Unpause(id download.TaskID) bool {
	t := d.mgr.Task(id)
	if t == nil {
		return false
	}
	t.Unpause()
	return true
}

// Remove cancels and forgets a task (best-effort).
func (d *Daemon) Remove(id download.TaskID) bool {
	t := d.mgr.Task(id)
	if t == nil {
		return false
	}
	t.Cancel()
	return true
}

// ChangeGlobalLimit updates the global rate limit at runtime. Used by RPC
// changeOption with "max-download-limit" key.
func (d *Daemon) ChangeGlobalLimit(spec string) error {
	return d.mgr.Limiter().SetRate(spec)
}

// ChangeTaskLimit updates the per-task rate limit for a running task. Used by
// RPC changeOption with "max-download-limit-per-task" key.
func (d *Daemon) ChangeTaskLimit(id download.TaskID, spec string) bool {
	t := d.mgr.Task(id)
	if t == nil {
		return false
	}
	return t.SetTaskRate(spec)
}

// ChangeConns adjusts the connection count for a running task. Used by RPC
// changeOption with "connections" key. Uses graceful drain for reduction;
// spawns new workers for increase. Returns false if task is not found or has
// already finished and cannot accept new workers.
func (d *Daemon) ChangeConns(id download.TaskID, newConns int) bool {
	t := d.mgr.Task(id)
	if t == nil {
		return false
	}
	return t.AdjustConns(newConns, nil, nil)
}

// TellStatus returns the snapshot of one task by id.
func (d *Daemon) TellStatus(id download.TaskID) (download.ProgressView, bool) {
	t := d.mgr.Task(id)
	if t == nil {
		return download.ProgressView{}, false
	}
	return t.Snapshot(), true
}

// TellActive returns snapshots of all currently-live tasks.
func (d *Daemon) TellActive() []download.ProgressView {
	return d.sch.LiveViews()
}

// TellWaiting returns snapshots of all queued tasks.
func (d *Daemon) TellWaiting() []download.ProgressView {
	return d.sch.QueuedViews()
}

// TellStopped returns snapshots of completed/failed tasks tracked since boot.
func (d *Daemon) TellStopped() []download.ProgressView {
	return d.sch.StoppedViews()
}

// OnComplete forwards to the underlying Scheduler's completion hook so the RPC
// Server can subscribe to per-task completion snapshots (spec
// onDownloadComplete / onDownloadError). Must be called before Start; setting
// it later is a no-op on the live scheduler (handleComplete snapshots the hook
// under its lock at fire time, but Start has already consumed the registration).
func (d *Daemon) OnComplete(f func(download.ProgressView)) {
	d.sch.OnComplete(f)
}
