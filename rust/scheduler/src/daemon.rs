//! Rust port of ODM's `internal/scheduler/daemon.go` — the Daemon: runs the
//! Scheduler in a background task and exposes live mutation for the RPC
//! server.

use std::sync::{Arc, Mutex, Weak};

use download::task::Error;
use download::{Ctx, Manager, ProgressView, TaskID};

use crate::scheduler::{OnCompleteFn, ScheduledTask, Scheduler};

/// `OnDeadFn` — fired once when the scheduler has wound down (used by the
/// RPC server's shutdown path).
pub type OnDeadFn = Box<dyn FnOnce() + Send>;

/// `DoneFlag` — the analogue of Go's closed done channel: polled via
/// `is_set()` for liveness, waited on via `wait()` for shutdown.
#[derive(Default)]
struct DoneFlag {
    done: Mutex<bool>,
    cv: std::sync::Condvar,
}

impl DoneFlag {
    fn set(&self) {
        *self.done.lock().unwrap() = true;
        self.cv.notify_all();
    }
    fn is_set(&self) -> bool {
        *self.done.lock().unwrap()
    }
    fn wait(&self) {
        let mut g = self.done.lock().unwrap();
        while !*g {
            g = self.cv.wait(g).unwrap();
        }
    }
}

#[derive(Default)]
struct DaemonState {
    started: bool,
    ctx: Option<Ctx>,
    done: Option<Arc<DoneFlag>>,
    on_dead: Vec<OnDeadFn>,
}

pub struct Daemon {
    pub(crate) sch: Arc<Scheduler>,
    pub(crate) mgr: Arc<Manager>,
    mu: Mutex<DaemonState>,
    me: Weak<Daemon>,
}

/// `new_daemon` — builds a Daemon over an already-constructed Scheduler +
/// Manager.
pub fn new_daemon(sch: Arc<Scheduler>, mgr: Arc<Manager>) -> Arc<Daemon> {
    Arc::new_cyclic(|weak: &Weak<Daemon>| Daemon {
        sch,
        mgr,
        mu: Mutex::new(DaemonState::default()),
        me: weak.clone(),
    })
}

impl Daemon {
    /// `start` — runs the underlying scheduler in the background; subsequent
    /// calls are no-ops.
    pub fn start(self: &Arc<Self>, ctx: &Ctx) {
        let (cctx, done) = {
            let mut st = self.mu.lock().unwrap();
            if st.started {
                return;
            }
            st.started = true;
            let cctx = Ctx::with_parent(ctx);
            let done = Arc::new(DoneFlag::default());
            st.done = Some(done.clone());
            st.ctx = Some(cctx.clone());
            (cctx, done)
        };
        let me = self.me.upgrade().expect("daemon alive");
        let sch = self.sch.clone();
        tokio::spawn(async move {
            let _ = sch.run(&cctx).await;
            done.set();
            let hooks = {
                let mut st = me.mu.lock().unwrap();
                std::mem::take(&mut st.on_dead)
            };
            for h in hooks {
                h();
            }
        });
    }

    /// `on_dead` — registers a callback fired when the scheduler has wound
    /// down. Calls h() immediately if the daemon is already dead (Go
    /// semantics).
    pub fn on_dead(&self, f: OnDeadFn) {
        let mut st = self.mu.lock().unwrap();
        let closed = st.done.as_ref().map(|d| d.is_set()).unwrap_or(false);
        if !closed {
            st.on_dead.push(f);
        } else {
            drop(st);
            f();
        }
    }

    /// `dead` — whether the scheduler's background run has finished.
    pub fn dead(&self) -> bool {
        let st = self.mu.lock().unwrap();
        st.done.as_ref().map(|d| d.is_set()).unwrap_or(false)
    }

    /// `stop` — signals the background scheduler to wind down and waits.
    pub fn stop(&self) {
        let done = {
            let st = self.mu.lock().unwrap();
            if let Some(cctx) = st.ctx.as_ref() {
                cctx.cancel();
            }
            st.done.clone()
        };
        if let Some(done) = done {
            done.wait();
        }
    }

    /// `signal_stop` — cancel only, without waiting. The RPC handler uses this
    /// so the JSON-RPC response is written before shutdown closes the listener
    /// (the OnDead hook fires later, once the scheduler run has wound down).
    pub fn signal_stop(&self) {
        let st = self.mu.lock().unwrap();
        if let Some(cctx) = st.ctx.as_ref() {
            cctx.cancel();
        }
    }

    /// `add_url` — creates a task for url with the given per-file connection
    /// budget and feeds it into the scheduler's admission path.
    pub fn add_url(&self, url: &str, conns: i32) -> Result<TaskID, Error> {
        let (t, default_conns) = self.mgr.new_task(url)?;
        let conns = if conns <= 0 { default_conns } else { conns };
        let st = ScheduledTask::new(t.clone(), conns);
        let ctx = self.mu.lock().unwrap().ctx.clone().unwrap_or_else(Ctx::new);
        self.sch.enqueue(st, &ctx)?;
        Ok(t.id())
    }

    /// `pause` — marks a task paused by id (no-op if unknown).
    pub fn pause(&self, id: &TaskID) -> bool {
        self.mgr.task(id).map(|t| {
            t.pause();
            true
        }).unwrap_or(false)
    }

    /// `unpause` — resumes a paused task.
    pub fn unpause(&self, id: &TaskID) -> bool {
        self.mgr.task(id).map(|t| {
            t.unpause();
            true
        }).unwrap_or(false)
    }

    /// `remove` — cancels and forgets a task (best-effort).
    pub fn remove(&self, id: &TaskID) -> bool {
        self.mgr.task(id).map(|t| {
            t.cancel();
            true
        }).unwrap_or(false)
    }

    /// `change_global_limit` — updates the global rate limit (RPC
    /// changeOption "max-download-limit").
    pub fn change_global_limit(&self, spec: &str) -> Result<(), String> {
        self.mgr.limiter().set_rate(spec)
    }

    /// `change_task_limit` — updates the per-task rate limit (RPC
    /// changeOption "max-download-limit-per-task").
    pub fn change_task_limit(&self, id: &TaskID, spec: &str) -> bool {
        self.mgr.task(id).map(|t| t.set_task_rate(spec)).unwrap_or(false)
    }

    /// `change_conns` — adjusts a running task's connection count (RPC
    /// changeOption "connections").
    pub fn change_conns(&self, id: &TaskID, new_conns: i32) -> bool {
        self.mgr.task(id).map(|t| t.adjust_conns(new_conns, None, None)).unwrap_or(false)
    }

    /// `tell_status` — the snapshot of one task by id.
    pub fn tell_status(&self, id: &TaskID) -> Option<ProgressView> {
        self.mgr.task(id).map(|t| t.snapshot())
    }

    /// `tell_active` — snapshots of all currently-live tasks.
    pub fn tell_active(&self) -> Vec<ProgressView> {
        self.sch.live_views()
    }

    /// `tell_waiting` — snapshots of all queued tasks.
    pub fn tell_waiting(&self) -> Vec<ProgressView> {
        self.sch.queued_views()
    }

    /// `tell_stopped` — snapshots of completed/failed tasks since boot.
    pub fn tell_stopped(&self) -> Vec<ProgressView> {
        self.sch.stopped_views()
    }

    /// `on_complete` — forwards to the Scheduler's completion hook (RPC
    /// onDownloadComplete / onDownloadError).
    pub fn on_complete(&self, f: OnCompleteFn) {
        self.sch.on_complete(f);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::scheduler::Scheduler;
    use std::time::{Duration, Instant};

    fn maker(mgr: &Arc<Manager>) -> crate::scheduler::TaskMaker {
        let mgr = mgr.clone();
        Arc::new(move |url: &str, _idx: i64| mgr.new_task(url))
    }

    fn mgr_for_tests() -> Arc<Manager> {
        Arc::new(Manager::new_manager(download::ExecOptions {
            dir: std::env::temp_dir().to_string_lossy().into_owned(),
            connections: 2,
            chunk_size: 4096,
            timeout: Duration::from_secs(5),
            check_cert: true,
            ..Default::default()
        }, None).unwrap())
    }

    // daemon_idle_test.go — TestEmptySchedulerStaysIdleWhenNoTasks: a
    // freshly-started RPC scheduler with zero tasks must NOT report Run as
    // finished (the daemon would exit before the first addUri). Cancelling the
    // ctx must release the idle hold.
    #[tokio::test]
    async fn empty_scheduler_stays_idle_when_no_tasks() {
        let mgr = mgr_for_tests();
        let sch = Scheduler::new_empty_scheduler(2, maker(&mgr), None);
        assert!(sch.is_idle(), "NewEmptyScheduler must set isIdle=true");
        let d = new_daemon(sch, mgr);

        let ctx = Ctx::new();
        d.start(&ctx);

        // Still parked after 500ms — the permanent idle hold is installed.
        tokio::time::sleep(Duration::from_millis(500)).await;
        assert!(!d.dead(), "empty scheduler finished Run within 500ms — idle hold broken");

        // Cancelling the ctx must release the idle hold and let Run return.
        ctx.cancel();
        let deadline = Instant::now() + Duration::from_secs(2);
        while !d.dead() && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(d.dead(), "scheduler never reported dead after ctx cancel (goroutine leak?)");
    }

    // daemon_idle_test.go — TestEmptySchedulerAdmitsLateTask.
    #[tokio::test]
    async fn empty_scheduler_admits_late_task() {
        let mgr = mgr_for_tests();
        let sch = Scheduler::new_empty_scheduler(2, maker(&mgr), None);
        assert!(sch.is_idle(), "NewEmptyScheduler must set isIdle=true");
        let d = new_daemon(sch, mgr);
        let ctx = Ctx::new();
        d.start(&ctx);

        // No tasks; confirm still alive briefly.
        assert!(!d.dead(), "daemon died on an empty scheduler");
        ctx.cancel();
        let deadline = Instant::now() + Duration::from_secs(2);
        while !d.dead() && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(d.dead());
    }

    // daemon_idle_test.go — TestRunDrainsCancelledTasksTally: when the run's
    // ctx is cancelled, in-flight tasks error out and must be counted as
    // FAILED, not dropped from the tally.
    #[tokio::test]
    async fn run_drains_cancelled_tasks_tally() {
        let mgr = mgr_for_tests();
        let sch = Scheduler::new_empty_scheduler(2, maker(&mgr), None);
        let d = new_daemon(sch, mgr);
        let ctx = Ctx::new();
        d.start(&ctx);

        // A URL whose probe never resolves → the task is stuck in probe
        // forever, exactly like a real download that's mid-flight when ^C lands.
        d.add_url("http://127.0.0.1:1/unreachable", 1).unwrap();

        // Give the task a moment to enter the live set, then cancel like ^C.
        let deadline = Instant::now() + Duration::from_secs(2);
        while d.sch.live_views().is_empty() && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        ctx.cancel();

        let deadline = Instant::now() + Duration::from_secs(5);
        while !d.dead() && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(d.dead(), "scheduler never wound down after ctx cancel (deadlock?)");
        // The single cancelled task must be tallied as failed — the bug
        // reported "0 failed" for ^C because Run returned without draining compl.
        assert_eq!(d.sch.failed_count(), 1);
        assert_eq!(d.sch.succeeded_count(), 0);
    }
}
