//! Rust port of ODM's `internal/download/task_pause.go`: SetTaskRate,
//! Pause/Unpause/pauseGate (RPC-facing hooks).

use std::sync::atomic::Ordering;
use std::sync::Arc;

use tokio::sync::Notify;

use crate::task::{Task, Ctx};

impl Task {
    /// `set_task_rate` — replaces the per-task limiter from a rate spec
    /// string ("2M", "off", "unlimited"). Returns whether the spec was valid.
    pub fn set_task_rate(&self, spec: &str) -> bool {
        match ratelimit::new(spec) {
            Ok(l) => {
                *self.task_lim.lock().unwrap() = Some(Arc::new(l));
                true
            }
            Err(_) => false,
        }
    }

    /// `pause` — sets the `paused` flag; workers rendezvous in pause_gate.
    pub fn pause(self: &Arc<Self>) {
        *self.paused.lock().unwrap() = true;
        self.set_state(crate::task::TaskState::Paused);
    }

    /// `unpause` — clears the flag and wakes every worker in pause_gate
    /// (broadcast-close wake-up: Notify is not a one-shot).
    pub fn unpause(self: &Arc<Self>) {
        {
            let mut paused = self.paused.lock().unwrap();
            if *paused {
                *paused = false;
            }
        }
        if let Some(n) = self.pause_c.lock().unwrap().as_ref() {
            n.notify_waiters();
        }
        self.set_state(crate::task::TaskState::Active);
    }

    /// `pause_gate` — blocks the calling worker while the task is paused.
    pub async fn pause_gate(self: &Arc<Self>, ctx: &Ctx) {
        loop {
            if !*self.paused.lock().unwrap() {
                return;
            }
            let n = {
                let mut guard = self.pause_c.lock().unwrap();
                if guard.is_none() {
                    *guard = Some(Arc::new(Notify::new()));
                }
                guard.as_ref().unwrap().clone()
            };
            let notified = n.notified();
            // Re-check after registration (Unpause may have fired meanwhile).
            if !*self.paused.lock().unwrap() {
                return;
            }
            tokio::select! {
                _ = ctx.wait() => return,
                _ = notified => {}
            }
        }
    }

    /// `cancel` — RPC-facing stop hook.
    pub fn cancel(&self) {
        self.cancelled.store(true, Ordering::SeqCst);
        if let Some(c) = self.cancel_ctx.lock().unwrap().clone() {
            c.cancel();
        }
    }
}
