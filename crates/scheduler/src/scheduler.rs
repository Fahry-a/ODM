//! Rust port of ODM's `internal/scheduler/queue.go` — the Scheduler: keeps
//! up to `slots` tasks running at any time, admitting queued tasks as live
//! slots become free.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, Weak};

use download::task::Error;
use download::{Ctx, ProgressView, Sink, Task, TaskID, TaskState};
use tokio::sync::mpsc::{unbounded_channel, UnboundedReceiver, UnboundedSender};

use crate::balancer::{Allocation, Plan};
use crate::rebalance::{rebalance_targets, RebalanceInput};

/// `TaskMaker` — builds a `Task` for a URL plus its per-file connection
/// budget. The Scheduler is decoupled from transport/config, so the Manager
/// provides this. `queue_idx == -1` means "queued slot" (informational).
pub type TaskMaker = Arc<dyn Fn(&str, i64) -> Result<(Arc<Task>, i32), Error> + Send + Sync>;

/// `ProgressCB` — forwards (live, queued) snapshots to the UI/RPC layer.
pub type ProgressCB = Arc<dyn Fn(Vec<ProgressView>, Vec<ProgressView>) + Send + Sync>;

/// `OnCompleteFn` — fired once per task as it leaves the live set, carrying
/// the task's final snapshot.
pub type OnCompleteFn = Arc<dyn Fn(ProgressView) + Send + Sync>;

/// `MAX_PENDING_TASKS` — bounds the scheduler's pending queue (RPC
/// backpressure; see Go's maxPendingTasks).
pub const MAX_PENDING_TASKS: usize = 128;

/// `MAX_STOPPED_TASKS` — bounds how many completed/failed tasks the
/// scheduler retains for tellStopped (Go's maxStoppedTasks).
static MAX_STOPPED_TASKS: AtomicUsize = AtomicUsize::new(1000);

/// `ScheduledTask` — pairs a download Task with its allocated connection
/// count. `counted` is true for a daemon task already added to the WaitGroup
/// while waiting in the queue; admission must not count it again.
#[derive(Clone)]
pub struct ScheduledTask {
    pub task: Arc<Task>,
    pub conns: i32,
    counted: Arc<AtomicBool>,
}

impl ScheduledTask {
    pub fn new(task: Arc<Task>, conns: i32) -> ScheduledTask {
        ScheduledTask { task, conns, counted: Arc::new(AtomicBool::new(false)) }
    }
    pub fn set_counted(&self) {
        self.counted.store(true, Ordering::SeqCst);
    }
    fn is_counted(&self) -> bool {
        self.counted.load(Ordering::SeqCst)
    }
}

/// `Wg` — a WaitGroup with a "wait until zero" built in (Go sync.WaitGroup
/// compiles to this shape: count + condvar; `wait` parks until count == 0).
struct Wg {
    count: Mutex<i64>,
    cv: std::sync::Condvar,
}

impl Wg {
    fn new() -> Wg {
        Wg { count: Mutex::new(0), cv: std::sync::Condvar::new() }
    }
    fn add(&self, n: i64) {
        *self.count.lock().unwrap() += n;
        self.cv.notify_all();
    }
    fn done(&self) {
        *self.count.lock().unwrap() -= 1;
        self.cv.notify_all();
    }
    fn wait(&self) {
        let mut c = self.count.lock().unwrap();
        while *c > 0 {
            c = self.cv.wait(c).unwrap();
        }
    }
}

/// `State` — everything Go's Scheduler holds under `mu`.
struct State {
    queued: Vec<ScheduledTask>,
    live: HashMap<TaskID, ScheduledTask>,
    stopped: Vec<ScheduledTask>,
    winding_down: bool,
    is_idle: bool,
    on_complete: Option<OnCompleteFn>,
}

/// `Scheduler` — the live scheduler driver: holds the plan, the task maker,
/// the slot count, and the run loop machinery (WaitGroup + completion
/// channel).
pub struct Scheduler {
    plan: Plan,
    slots: i32,
    maker: TaskMaker,
    prog: Option<ProgressCB>,

    mu: Mutex<State>,
    wg: Arc<Wg>,
    compl_tx: UnboundedSender<ScheduledTask>,
    compl_rx: Mutex<Option<UnboundedReceiver<ScheduledTask>>>,
    stop_released: AtomicBool,
    succeeded: AtomicI64,
    failed: AtomicI64,

    me: Mutex<Option<Weak<Scheduler>>>,
}

impl Scheduler {
    /// `new_scheduler` — constructs a Scheduler from a Balancer Plan and a
    /// TaskMaker. The one-shot CLI path drives it directly via `run`.
    pub fn new_scheduler(plan: Plan, maker: TaskMaker, prog: Option<ProgressCB>) -> Arc<Scheduler> {
        let slots = plan.parallel.len() as i32;
        Self::build(plan, slots, maker, prog, false)
    }

    /// `new_empty_scheduler` — builds a Scheduler with no initial tasks but a
    /// fixed slot count; the RPC daemon uses this (add_url populates the
    /// queue later). The Balancer rejects an empty file list, hence this
    /// constructor. A daemon scheduler with an empty queue must NOT report
    /// Run as finished instantly (Go comment) — a permanent idle hold is
    /// installed here and released only on ctx cancellation inside run().
    pub fn new_empty_scheduler(slots: i32, maker: TaskMaker, prog: Option<ProgressCB>) -> Arc<Scheduler> {
        Self::build(Plan::default(), slots, maker, prog, true)
    }

    fn build(plan: Plan, slots: i32, maker: TaskMaker, prog: Option<ProgressCB>, is_idle: bool) -> Arc<Scheduler> {
        let (compl_tx, compl_rx) = unbounded_channel();
        let wg = Arc::new(Wg::new());
        if is_idle {
            wg.add(1);
        }
        let s = Arc::new(Scheduler {
            plan,
            slots,
            maker,
            prog,
            mu: Mutex::new(State {
                queued: Vec::new(),
                live: HashMap::new(),
                stopped: Vec::new(),
                winding_down: false,
                is_idle,
                on_complete: None,
            }),
            wg,
            compl_tx,
            compl_rx: Mutex::new(Some(compl_rx)),
            stop_released: AtomicBool::new(false),
            succeeded: AtomicI64::new(0),
            failed: AtomicI64::new(0),
            me: Mutex::new(None),
        });
        *s.me.lock().unwrap() = Some(Arc::downgrade(&s));
        s
    }

    /// `on_complete` — registers a callback fired once per task as it leaves
    /// the live set, carrying the task's final snapshot.
    pub fn on_complete(&self, f: OnCompleteFn) {
        self.mu.lock().unwrap().on_complete = Some(f);
    }

    /// `run` — blocks until every task (parallel + queued) finishes or ctx is
    /// cancelled. Returns aggregate (succeeded, failed) tallies (Go's err is
    /// always nil for one-shot runs and ignored by daemon).
    pub async fn run(self: &Arc<Self>, ctx: &Ctx) -> (i64, i64) {
        // 1. Build the queue (everything starts as "waiting" until a slot is granted).
        for a in &self.plan.queued {
            match (self.maker)(&a.url, -1) {
                Err(_) => {
                    self.failed.fetch_add(1, Ordering::SeqCst);
                }
                Ok((t, _)) => {
                    self.mu.lock().unwrap().queued.push(ScheduledTask::new(t, a.connections));
                }
            }
        }

        // 2. Admit up to `slots` initial tasks (Go: loop with admitted<slots).
        let initial: Vec<Allocation> = self.plan.parallel.clone();
        let mut admitted = 0i32;
        for (i, a) in initial.iter().enumerate() {
            if admitted >= self.slots {
                break;
            }
            match (self.maker)(&a.url, i as i64) {
                Err(_) => {
                    self.failed.fetch_add(1, Ordering::SeqCst);
                }
                Ok((t, _)) => {
                    if !self.add_counter() {
                        continue; // winding down; don't launch new work
                    }
                    self.start_one(ctx, ScheduledTask::new(t, a.connections));
                    admitted += 1;
                }
            }
        }

        // 3. Wait for all admitted (and later-queued) tasks. The WaitGroup
        //    waits on a separate path — not a bare wg.wait() — so the run
        //    loop can keep servicing compl while tasks wind down.
        let wg = self.wg.clone();
        let mut done = tokio::task::spawn_blocking(move || wg.wait());
        let mut compl_rx = self.compl_rx.lock().unwrap().take().expect("compl_rx consumed twice");
        loop {
            tokio::select! {
                _ = &mut done => {
                    eprintln!("run: done fired");
                    // Everything (live + queued) finished without cancellation.
                    // Drain completion reports still sitting in the buffer
                    // (launch posts AFTER wg.done, so with two tasks
                    // finishing back-to-back the select can pick done while
                    // the last report waits). Keep the tally complete.
                    while let Ok(st) = compl_rx.try_recv() {
                        self.handle_complete(&st);
                    }
                    eprintln!("run: returning");
                    return (self.succeeded.load(Ordering::SeqCst), self.failed.load(Ordering::SeqCst));
                }
                r = compl_rx.recv() => {
                    if let Some(st) = r {
                        self.handle_complete(&st);
                        self.admit_next(ctx);
                        self.rebalance_live(ctx);
                        self.emit();
                    }
                    continue;
                }
                _ = ctx.wait() => {
                    // Mark winding_down BEFORE release_idle drops the idle hold
                    // so no concurrent RPC enqueue can race the WaitGroup wake.
                    // Queued daemon tasks that were counted have no worker
                    // goroutine, so release their WaitGroup reservations too.
                    let queued_counted = {
                        let mut mu = self.mu.lock().unwrap();
                        mu.winding_down = true;
                        let n = mu.queued.iter().filter(|q| q.is_counted()).count();
                        mu.queued.clear();
                        n
                    };
                    for _ in 0..queued_counted {
                        self.wg.done();
                    }
                    self.release_idle();
                    // Drain completion reports until no task is left live.
                    while self.active_tasks() > 0 {
                        match compl_rx.recv().await {
                            Some(st) => {
                                self.handle_complete(&st);
                                self.emit();
                            }
                            None => break,
                        }
                    }
                    eprintln!("run: cancel path returning");
                    return (self.succeeded.load(Ordering::SeqCst), self.failed.load(Ordering::SeqCst));
                }
            }
        }
    }

    /// `active_tasks` — count of tasks currently in the live map.
    fn active_tasks(&self) -> usize {
        self.mu.lock().unwrap().live.len()
    }

    /// `add_counter` — WaitGroup increment under the run-loop's knowledge of
    /// whether shutdown is in progress. Refuses once Run has started winding
    /// down (the Add-vs-Wait race fix).
    fn add_counter(&self) -> bool {
        let mu = self.mu.lock().unwrap();
        if mu.winding_down {
            return false;
        }
        self.wg.add(1);
        true
    }

    /// `release_idle` — drops the permanent WaitGroup hold installed by
    /// new_empty_scheduler exactly once (stopOnce).
    fn release_idle(&self) {
        if self.stop_released.swap(true, Ordering::SeqCst) {
            return;
        }
        let mut mu = self.mu.lock().unwrap();
        if mu.is_idle {
            self.wg.done();
            mu.is_idle = false;
        }
    }

    /// `is_idle` — whether the permanent idle hold is installed (daemon mode).
    pub fn is_idle(&self) -> bool {
        self.mu.lock().unwrap().is_idle
    }

    /// `start_one` — launches a task with its allocated conns.
    fn start_one(self: &Arc<Self>, ctx: &Ctx, st: ScheduledTask) {
        st.task.set_conns(st.conns);
        self.mu.lock().unwrap().live.insert(st.task.id(), st.clone());
        self.launch(ctx, st);
    }

    /// `launch` — runs the task on a spawned task; posts the completion
    /// report to compl after the task returns (Go: `defer s.wg.Done()`).
    fn launch(self: &Arc<Self>, ctx: &Ctx, st: ScheduledTask) {
        let task = st.task.clone();
        let conns = st.conns;
        let tx = self.compl_tx.clone();
        let wg = self.wg.clone();
        let me = self.me.lock().unwrap().as_ref().unwrap().upgrade().unwrap();
        let sink: Option<Sink> = Some(Arc::new(move |_v: ProgressView| me.emit()));
        let ctx2 = ctx.clone();
        tokio::spawn(async move {
            let _ = task.start(&ctx2, conns, sink).await;
            let _ = tx.send(st);
            wg.done();
            eprintln!("launch: wg.done");
        });
    }

    /// `handle_complete` — tallies the result and retires the slot.
    fn handle_complete(&self, st: &ScheduledTask) {
        let cb = {
            let mut mu = self.mu.lock().unwrap();
            mu.live.remove(&st.task.id());
            mu.stopped.push(st.clone());
            let max_t = MAX_STOPPED_TASKS.load(Ordering::SeqCst);
            if max_t > 0 && mu.stopped.len() > max_t {
                let keep = mu.stopped.len() - max_t;
                mu.stopped.drain(..keep);
            }
            mu.on_complete.clone()
        };
        if st.task.state() == TaskState::Completed {
            self.succeeded.fetch_add(1, Ordering::SeqCst);
        } else {
            self.failed.fetch_add(1, Ordering::SeqCst);
        }
        // Lifecycle events fire post-tally so the snapshot reads the final state.
        if let Some(cb) = cb {
            cb(st.task.snapshot());
        }
    }

    /// `enqueue` — injects an externally-built task (RPC addUri) into the
    /// queue. The task is counted exactly once while waiting; admit_next
    /// consumes that existing reservation when the task gets a live slot.
    pub fn enqueue(self: &Arc<Self>, st: ScheduledTask, ctx: &Ctx) -> Result<(), String> {
        {
            let mut mu = self.mu.lock().unwrap();
            if mu.winding_down {
                return Err("scheduler is shutting down".to_string());
            }
            if mu.queued.len() >= MAX_PENDING_TASKS {
                return Err("too many pending tasks".to_string());
            }
            // Reserve the WaitGroup count under the same lock used by
            // admit_next and the shutdown flag. This prevents a queued task
            // from being counted twice if an admission races the RPC enqueue.
            self.wg.add(1);
            st.set_counted();
            mu.queued.push(st);
        }
        self.admit_next(ctx);
        Ok(())
    }

    /// `live_views` — snapshots of currently running tasks (sorted by ID).
    pub fn live_views(&self) -> Vec<ProgressView> {
        let mut out: Vec<ProgressView> =
            self.mu.lock().unwrap().live.values().map(|st| st.task.snapshot()).collect();
        out.sort_by(|a, b| a.id.cmp(&b.id));
        out
    }

    /// `queued_views` — snapshots of waiting tasks (insertion order).
    pub fn queued_views(&self) -> Vec<ProgressView> {
        self.mu.lock().unwrap().queued.iter().map(|st| st.task.snapshot()).collect()
    }

    /// `stopped_views` — snapshots of completed/failed tasks tracked since boot.
    pub fn stopped_views(&self) -> Vec<ProgressView> {
        self.mu.lock().unwrap().stopped.iter().map(|st| st.task.snapshot()).collect()
    }

    /// `succeeded_count` — live scheduler tally used by tests and daemon lifecycle checks.
    pub fn succeeded_count(&self) -> i64 {
        self.succeeded.load(Ordering::SeqCst)
    }

    /// `failed_count` — live scheduler tally (Go counts.go).
    pub fn failed_count(&self) -> i64 {
        self.failed.load(Ordering::SeqCst)
    }

    fn admit_next(self: &Arc<Self>, ctx: &Ctx) {
        let nxt = {
            let mut mu = self.mu.lock().unwrap();
            if mu.queued.is_empty() || mu.live.len() >= self.slots as usize {
                return;
            }
            let nxt = mu.queued.remove(0);
            nxt.task.set_conns(nxt.conns);
            mu.live.insert(nxt.task.id(), nxt.clone());
            nxt
        };
        if !nxt.is_counted() {
            if !self.add_counter() {
                // Winding down — the task was already moved to live; remove it
                // again so the bookkeeping stays consistent (the daemon is
                // exiting anyway).
                self.mu.lock().unwrap().live.remove(&nxt.task.id());
                return;
            }
        }
        self.launch(ctx, nxt);
    }

    /// `emit` — forwards a snapshot to the progress callback (nil-safe).
    fn emit(&self) {
        if let Some(f) = &self.prog {
            f(self.live_views(), self.queued_views());
        }
    }

    /// `rebalance_live` — moves the Mode C connection budget across currently
    /// live tasks. Called by the scheduler loop after a completion/admission
    /// event, so the operation is serialized with other scheduler decisions.
    fn rebalance_live(&self, ctx: &Ctx) {
        if !self.plan.rebalance_connections || self.plan.connection_budget < 1 {
            return;
        }
        let mut inputs: Vec<RebalanceInput> = {
            let mu = self.mu.lock().unwrap();
            if mu.live.is_empty() {
                return;
            }
            mu.live
                .values()
                .map(|st| RebalanceInput {
                    id: st.task.id(),
                    range_capable: !st.task.snapshot().single_stream,
                    task: st.task.clone(),
                })
                .collect()
        };
        inputs.sort_by(|a, b| a.id.cmp(&b.id));
        let targets = rebalance_targets(&inputs, self.plan.connection_budget);
        for (i, inp) in inputs.iter().enumerate() {
            let tgt = targets[i];
            if tgt < 1 {
                continue;
            }
            let task = &inp.task;
            if !task.adjust_conns(tgt, Some(ctx.clone()), None) {
                // The task may be mid-Start (rejecting adjust_conns) —
                // set_conns is safe before start and preserves conn_target.
                let view = task.snapshot();
                if view.state != TaskState::Completed && view.state != TaskState::Error {
                    task.set_conns(tgt);
                }
            }
            let mut mu = self.mu.lock().unwrap();
            if let Some(live) = mu.live.get_mut(&inp.id) {
                if live.task.id() == inp.id {
                    live.conns = tgt;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicI64;
    use std::time::{Duration, Instant};

    use crate::daemon::new_daemon;
    use download::Manager;
    use crate::balancer::{compute, FileInput, Plan};

    fn maker(mgr: &Arc<Manager>) -> TaskMaker {
        let mgr = mgr.clone();
        Arc::new(move |url: &str, _idx: i64| mgr.new_task(url))
    }

    fn mgr_for_tests() -> Arc<Manager> {
        Arc::new(Manager::new_manager(download::ExecOptions {
            dir: std::env::temp_dir().to_string_lossy().into_owned(),
            connections: 1,
            chunk_size: 4096,
            retry: 0,
            timeout: Duration::from_secs(10),
            check_cert: true,
            ..Default::default()
        }, None).unwrap())
    }

    // compl_drain_test.go — TestRun_TallyIncludesLastCompletions: launch posts
    // a task's completion to compl AFTER wg.done, so when the last two tasks
    // finish back-to-back, run's select can pick done while the final report
    // is still in the buffer. The drain must keep the tally complete.
    #[tokio::test]
    async fn run_tallies_all_finished_tasks() {
        for _iter in 0..30 {
            let mgr = mgr_for_tests();
            let completed = Arc::new(AtomicI64::new(0));
            let sch = Scheduler::new_scheduler(
                Plan {
                    parallel: vec![
                        Allocation { url: "http://example.invalid/a.bin".into(), connections: 1, supports_range: true },
                        Allocation { url: "http://example.invalid/b.bin".into(), connections: 1, supports_range: true },
                    ],
                    ..Default::default()
                },
                maker(&mgr),
                None,
            );
            let c = completed.clone();
            sch.on_complete(Arc::new(move |_v: ProgressView| {
                let n = c.fetch_add(1, Ordering::SeqCst) + 1;
                assert!(n <= 2, "more than 2 completion events");
            }));
            let (ok, failed) = sch.run(&Ctx::new()).await;
            assert_eq!(ok + failed, 2, "run tallied {} of 2 finished tasks", ok + failed);
            assert_eq!(completed.load(Ordering::SeqCst), 2, "completion events");
        }
    }

    // admission_test.go — TestEnqueue_RejectsAfterShutdown.
    #[tokio::test]
    async fn enqueue_rejects_after_shutdown() {
        let mgr = mgr_for_tests();
        let sch = Scheduler::new_empty_scheduler(1, maker(&mgr), None);
        let d = new_daemon(sch.clone(), mgr.clone());
        let ctx = Ctx::new();
        d.start(&ctx);

        // Cancel the context to trigger winding-down.
        ctx.cancel();

        // Wait for the scheduler to observe the cancellation.
        let deadline = Instant::now() + Duration::from_secs(3);
        while !d.dead() && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(d.dead(), "scheduler should be dead after cancel");

        let (t, _) = mgr.new_task("http://example.invalid/x").unwrap();
        assert!(sch.enqueue(ScheduledTask::new(t, 1), &ctx).is_err());
    }

    // admission_test.go — TestEnqueue_RejectsWhenQueueFull (0-slot scheduler
    // so nothing drains: every Enqueue stays queued to the limit).
    #[tokio::test]
    async fn enqueue_rejects_when_queue_full() {
        let mgr = mgr_for_tests();
        let sch = Scheduler::new_empty_scheduler(0, maker(&mgr), None);
        let ctx = Ctx::new();

        let mut filled = 0;
        for _ in 0..MAX_PENDING_TASKS {
            let (t, _) = mgr.new_task("http://example.invalid/fill").unwrap();
            sch.enqueue(ScheduledTask::new(t, 1), &ctx).expect("fill enqueue");
            filled += 1;
        }
        assert_eq!(filled, MAX_PENDING_TASKS);

        // The next Enqueue must fail.
        let (t, _) = mgr.new_task("http://example.invalid/overflow").unwrap();
        assert!(sch.enqueue(ScheduledTask::new(t, 1), &ctx).is_err());
    }

    // admission_test.go — TestEnqueue_ConcurrentEnqueue.
    #[tokio::test]
    async fn enqueue_concurrent_enqueue() {
        let mgr = mgr_for_tests();
        let sch = Scheduler::new_empty_scheduler(0, maker(&mgr), None);
        let ctx = Ctx::new();

        let err_count = Arc::new(AtomicI64::new(0));
        let mut joins = Vec::new();
        for _ in 0..200 {
            let sch = sch.clone();
            let mgr = mgr.clone();
            let ctx = ctx.clone();
            let err_count = err_count.clone();
            joins.push(tokio::spawn(async move {
                let (t, _) = mgr.new_task("http://example.invalid/concurrent").unwrap();
                if sch.enqueue(ScheduledTask::new(t, 1), &ctx).is_err() {
                    err_count.fetch_add(1, Ordering::SeqCst);
                }
            }));
        }
        for j in joins {
            j.await.unwrap();
        }

        // Some enqueues must have been rejected (200 > 128).
        assert!(err_count.load(Ordering::SeqCst) > 0, "expected some concurrent enqueues to be rejected");
        assert!(sch.queued_views().len() <= MAX_PENDING_TASKS);
    }

    // admission_test.go — TestEnqueue_ShutdownRace.
    #[tokio::test]
    async fn enqueue_shutdown_race() {
        for _ in 0..50 {
            let mgr = mgr_for_tests();
            let sch = Scheduler::new_empty_scheduler(1, maker(&mgr), None);
            let d = new_daemon(sch, mgr);
            let ctx = Ctx::new();
            d.start(&ctx);

            // Goroutine A: repeatedly AddURL until shutdown.
            let d_a = d.clone();
            let a = tokio::spawn(async move {
                for _ in 0..20 {
                    let _ = d_a.add_url("http://example.invalid/race", 1);
                }
            });
            // Goroutine B: cancel after a short delay.
            let ctx2 = ctx.clone();
            let b = tokio::spawn(async move {
                tokio::time::sleep(Duration::from_micros(1)).await;
                ctx2.cancel();
            });
            let _ = a.await;
            let _ = b.await;

            let deadline = Instant::now() + Duration::from_secs(3);
            while !d.dead() && Instant::now() < deadline {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
            assert!(d.dead());
        }
    }

    // -- Balancer --
    #[test]
    fn compute_modes() {
        let files = |n: usize| (0..n).map(|_| FileInput { url: "u".into(), supports_range: true }).collect::<Vec<_>>();
        let total = |p: &Plan| p.parallel.iter().map(|a| a.connections).sum::<i32>();

        // Mode A.
        let p = compute(16, &files(1), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 1);
        assert_eq!(p.parallel[0].connections, 16);
        assert_eq!(p.queued.len(), 0);
        let p = compute(100, &files(1), 0, 32).unwrap();
        assert_eq!(p.parallel[0].connections, 32);
        assert!(!p.warning.is_empty());
        // Mode B.
        let p = compute(16, &files(16), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 16);
        assert_eq!(p.queued.len(), 0);
        let p = compute(8, &files(16), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 8);
        assert_eq!(p.queued.len(), 8);
        let p = compute(64, &files(64), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 32);
        assert_eq!(p.queued.len(), 32);
        // Mode C.
        let p = compute(16, &files(10), 5, 32).unwrap();
        assert_eq!(p.parallel.len(), 3);
        assert_eq!(p.queued.len(), 7);
        assert_eq!(total(&p), 16);
        // Validation.
        assert!(compute(0, &files(1), 0, 32).is_err());
        assert!(compute(4, &files(8), 5, 32).is_err());
    }
}

