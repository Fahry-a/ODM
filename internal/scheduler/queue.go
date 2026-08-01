// queue.go is the dynamic scheduling layer (PRD §5.3, §5.4): it takes the
// Balancer's static plan and turns it into a live scheduler that keeps up to
// `len(plan.Parallel)` tasks running at any time, advancing queued tasks as
// slots free up. This is the "rest queued, auto-start as slots free" behaviour.
package scheduler

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"odm/internal/download"
)

// Scheduler runs a batch (or single-file) download honouring the Balancer's
// allocation. Initial parallel set comes from plan.Parallel; plan.Queued tasks
// are admitted one-for-one as live slots become free. Each queued task inherits
// the same per-file connection budget (Mode C: SF; Mode B: 1).
type Scheduler struct {
	plan  *Plan
	maker TaskMaker
	slots int // len(plan.Parallel)
	prog  ProgressCB

	// onComplete is fired once per task as it leaves the live set
	// (handleComplete), carrying the task's final snapshot. The RPC Server wires
	// this (PRD §10.3) to emit onDownloadComplete / onDownloadError. nil ⇒ off
	// (the CLI one-shot path never sets it). Guarded by mu below.
	onComplete func(download.ProgressView)

	mu       sync.Mutex
	queued   []*scheduledTask
	live     map[download.TaskID]*scheduledTask
	stopped  []*scheduledTask // completed/failed, kept for tellStopped
	wg       sync.WaitGroup
	compl    chan scheduledTask // finished-task signals
	stopOnce sync.Once

	// idle, when true, means a permanent WaitGroup hold has been added to wg
	// (in daemon mode, NewEmptyScheduler) so an empty scheduler doesn't report
	// "all done" and exit before the first RPC addUri arrives. The hold lives
	// on wg itself — not a separate WaitGroup — so the wg.Wait() observer in
	// Run stays parked until releaseIdle drops it on ctx cancellation.
	isIdle bool

	succeeded int32
	failed    int32
}

// scheduledTask pairs a download.Task with its allocated connection count.
type scheduledTask struct {
	task  *download.Task
	conns int
}

// ProgressCB forwards a snapshot of live + queued tasks to the UI/RPC layer.
type ProgressCB func(live, queued []download.ProgressView)

// TaskMaker builds a *download.Task for a URL plus its per-file connection
// budget. The Scheduler is decoupled from transport/config, so the Manager
// provides this. queueIdx==-1 means "queued slot" (informational).
type TaskMaker func(url string, queueIdx int) (*download.Task, int, error)

// NewScheduler constructs a Scheduler from a Balancer Plan and a TaskMaker.
func NewScheduler(plan *Plan, maker TaskMaker, prog ProgressCB) *Scheduler {
	return &Scheduler{
		plan:  plan,
		maker: maker,
		slots: len(plan.Parallel),
		prog:  prog,
		live:  map[download.TaskID]*scheduledTask{},
		compl: make(chan scheduledTask, 1),
	}
}

// OnComplete registers a callback fired exactly once per task as it leaves the
// live set (handleComplete), carrying the task's final snapshot. The RPC Server
// uses it to emit onDownloadComplete / onDownloadError (PRD §10.3). May only be
// called before Run; it is a no-op to set on a scheduler already running. The
// one-shot CLI path leaves it nil — completion is surfaced only via the summary.
func (s *Scheduler) OnComplete(f func(download.ProgressView)) {
	s.mu.Lock()
	s.onComplete = f
	s.mu.Unlock()
}

// NewEmptyScheduler builds a Scheduler with no initial tasks but a fixed slot
// count; it's the form the RPC daemon uses (addUri populates the queue later).
// The Balancer itself rejects an empty file list, hence this constructor.
//
// Unlike the one-shot NewScheduler, a daemon scheduler with an empty queue
// must NOT report "all work done" the instant Run is entered — otherwise the
// daemon exits before the first RPC addUri arrives. A permanent idle hold is
// installed here and released only on ctx cancellation inside Run.
func NewEmptyScheduler(slots int, maker TaskMaker, prog ProgressCB) *Scheduler {
	s := &Scheduler{
		plan:   &Plan{MaxConnections: DefaultMaxConnections},
		maker:  maker,
		slots:  slots,
		prog:   prog,
		live:   map[download.TaskID]*scheduledTask{},
		compl:  make(chan scheduledTask, 1),
		isIdle: true,
	}
	// Permanent hold on wg so Run() stays parked (idle) until the daemon's ctx
	// is cancelled. Released by releaseIdle, which OnDead's caller triggers.
	s.wg.Add(1)
	return s
}

// Run blocks until every task (parallel + queued) finishes, or ctx is cancelled.
// Returns aggregate succeeded/failed counts.
//
// s.compl is created by the constructors, not here: the RPC daemon can Enqueue
// a task (and its launch goroutine starts reading s.compl) the instant Start
// returns, racing Run's setup — assigning the channel here would be a data race
// between Run and the first admitted task.
func (s *Scheduler) Run(ctx context.Context) (succeeded, failed int, err error) {
	// Build the queue (everything starts as "waiting" until a slot is granted).
	for _, a := range s.plan.Queued {
		t, _, mErr := s.maker(a.URL, -1)
		if mErr != nil {
			atomic.AddInt32(&s.failed, 1)
			continue
		}
		s.mu.Lock()
		s.queued = append(s.queued, &scheduledTask{task: t, conns: a.Connections})
		s.mu.Unlock()
	}

	// Admit up to `slots` initial tasks.
	s.mu.Lock()
	initial := s.plan.Parallel
	s.mu.Unlock()
	admitted := 0
	for i := 0; i < len(initial) && admitted < s.slots; i++ {
		t, _, mErr := s.maker(initial[i].URL, i)
		if mErr != nil {
			atomic.AddInt32(&s.failed, 1)
			continue
		}
		s.wg.Add(1)
		// Use the Balancer's per-file allocation, not the TaskMaker's global default.
		s.startOne(ctx, &scheduledTask{task: t, conns: initial[i].Connections})
		admitted++
	}

	// Wait for all admitted (and later-queued) tasks.
	doneCh := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(doneCh)
	}()

	for {
		select {
		case <-ctx.Done():
			// Release the daemon idle hold (if any) so bg wg.Wait returns and
			// Run's goroutine can exit; then cancel still-propagating work.
			s.releaseIdle()
			// Brief wait so running tasks have a chance to persist control
			// files (with completed chunk offsets) before the process exits
			// via os.Exit in main(). A full doneCh wait is avoided because
			// queued tasks may have unbalanced wg.Add that prevents wg.Wait
			// from ever returning.
			select {
			case <-doneCh:
			case <-time.After(200 * time.Millisecond):
			}
			return int(s.succeeded), int(s.failed), ctx.Err()
		case st := <-s.compl:
			s.handleComplete(st)
			s.admitNext(ctx)
			s.emit()
		case <-doneCh:
			return int(s.succeeded), int(s.failed), nil
		}
	}
}

// releaseIdle drops the permanent WaitGroup hold installed by
// NewEmptyScheduler exactly once. Used when the daemon's ctx is cancelled so
// the background wg.Wait() goroutine finally observes "no outstanding work"
// and Run can return. Also safe on the one-shot path (isIdle false) where it's
// a no-op.
func (s *Scheduler) releaseIdle() {
	s.stopOnce.Do(func() {
		if s.isIdle {
			s.wg.Done()
			s.isIdle = false
		}
	})
}

// startOne launches a task with its allocated conns. When it finishes it posts
// itself to s.compl and decrements the WaitGroup.
func (s *Scheduler) startOne(ctx context.Context, st *scheduledTask) {
	st.task.SetConns(st.conns)

	s.mu.Lock()
	s.live[st.task.ID()] = st
	s.mu.Unlock()

	s.launch(ctx, st)
}

// launch runs the task goroutine (the WaitGroup was already counted by the
// caller) and reports its completion back on s.compl.
func (s *Scheduler) launch(ctx context.Context, st *scheduledTask) {
	go func() {
		defer s.wg.Done()
		_ = st.task.Start(ctx, st.conns, func(download.ProgressView) { s.emit() })
		s.compl <- *st
	}()
}

// maxStoppedTasks bounds how many completed/failed tasks the scheduler retains
// for tellStopped. Without a cap a long-lived daemon would accumulate every
// finished task in memory forever; once exceeded, the oldest are dropped
// (a fresh slice is allocated so the backing array's pointers are released).
var maxStoppedTasks = 1000

// handleComplete tallies the result and retires the slot.
func (s *Scheduler) handleComplete(st scheduledTask) {
	s.mu.Lock()
	delete(s.live, st.task.ID())
	s.stopped = append(s.stopped, &st)
	if maxStoppedTasks > 0 && len(s.stopped) > maxStoppedTasks {
		keep := len(s.stopped) - maxStoppedTasks
		s.stopped = append([]*scheduledTask(nil), s.stopped[keep:]...)
	}
	cb := s.onComplete
	s.mu.Unlock()
	if st.task.State() == download.StateCompleted {
		atomic.AddInt32(&s.succeeded, 1)
	} else {
		atomic.AddInt32(&s.failed, 1)
	}
	// §10.3 lifecycle events: fire post-tally so the snapshot reads the final
	// state (completed vs. error). cb is nil on the CLI one-shot path.
	if cb != nil {
		cb(st.task.Snapshot())
	}
}

// Enqueue injects an externally-built task (RPC addUri) into the queue. Slot
// admission happens the same way as batch-queued tasks. ctx is passed through
// to startOne so the task honours daemon cancellation. Used only in daemon
// mode; the CLI one-shot path never calls this.
func (s *Scheduler) Enqueue(st *scheduledTask, ctx context.Context) {
	s.mu.Lock()
	s.queued = append(s.queued, st)
	s.mu.Unlock()
	s.wg.Add(1)
	s.admitNext(ctx)
}

// LiveViews returns snapshots of currently running tasks.
func (s *Scheduler) LiveViews() []download.ProgressView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]download.ProgressView, 0, len(s.live))
	for _, st := range s.live {
		out = append(out, st.task.Snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// QueuedViews returns snapshots of waiting tasks.
func (s *Scheduler) QueuedViews() []download.ProgressView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]download.ProgressView, 0, len(s.queued))
	for _, st := range s.queued {
		out = append(out, st.task.Snapshot())
	}
	return out
}

// StoppedViews returns snapshots of completed/failed tasks tracked since boot.
func (s *Scheduler) StoppedViews() []download.ProgressView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]download.ProgressView, 0, len(s.stopped))
	for _, st := range s.stopped {
		out = append(out, st.task.Snapshot())
	}
	return out
}

// admitNext starts one queued task if there's a free slot. The free-slot check,
// the queue pop and the live-map insert happen in ONE critical section, so two
// concurrent admitters (the Run loop and an RPC Enqueue) can't both pass the
// check before either has registered its task — that TOCTOU used to let live
// temporarily exceed slots by one. With the slot reserved atomically, later
// completions and the slot count stay consistent.
func (s *Scheduler) admitNext(ctx context.Context) {
	s.mu.Lock()
	if len(s.queued) == 0 || len(s.live) >= s.slots {
		s.mu.Unlock()
		return
	}
	nxt := s.queued[0]
	s.queued = s.queued[1:]
	nxt.task.SetConns(nxt.conns)
	s.live[nxt.task.ID()] = nxt
	s.mu.Unlock()

	s.wg.Add(1)
	s.launch(ctx, nxt)
}

// emit forwards a snapshot to the progress callback (nil-safe).
func (s *Scheduler) emit() {
	if s.prog == nil {
		return
	}
	s.mu.Lock()
	live := make([]download.ProgressView, 0, len(s.live))
	for _, st := range s.live {
		live = append(live, st.task.Snapshot())
	}
	queued := make([]download.ProgressView, 0, len(s.queued))
	for _, st := range s.queued {
		queued = append(queued, st.task.Snapshot())
	}
	s.mu.Unlock()
	// Sort live by TaskID for deterministic ordering (Go map iteration is
	// random, so without this the task lines shuffle between frames).
	sort.Slice(live, func(i, j int) bool {
		return live[i].ID < live[j].ID
	})
	s.prog(live, queued)
}

// SucceededCount/FailedCount expose live tallies (used by getGlobalStat RPC).
func (s *Scheduler) SucceededCount() int { return int(atomic.LoadInt32(&s.succeeded)) }
func (s *Scheduler) FailedCount() int    { return int(atomic.LoadInt32(&s.failed)) }
