//! Rust port of ODM's `internal/rpc/throttler.go` — coalesces high-frequency
//! scheduler progress snapshots into at most one WebSocket event per 250ms.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use download::ProgressView;
use scheduler::ProgressCB;

use crate::server::snapshot_params;
use crate::types::Event;
use crate::ws::Broadcaster;

const THROTTLE_INTERVAL: Duration = Duration::from_millis(250);

/// `ProgressThrottler` wraps a Broadcaster with a ~250ms emission floor.
pub struct ProgressThrottler {
    bc: Arc<Broadcaster>,
    mu: Mutex<Instant>,
}

impl ProgressThrottler {
    pub fn new(bc: Arc<Broadcaster>) -> Arc<ProgressThrottler> {
        // Go's zero-value `last` is in the past, so the first Forward always
        // emits. An Instant in the past is simulated with `Instant::now() -
        // THROTTLE_INTERVAL`.
        Arc::new(ProgressThrottler { bc, mu: Mutex::new(Instant::now() - THROTTLE_INTERVAL) })
    }

    /// `forward` — returns a `scheduler.ProgressCB`. Emits one
    /// `onDownloadProgress` per live task at most once per tick; queued tasks
    /// are skipped (they have no live bytes yet).
    pub fn callback(self: &Arc<Self>) -> ProgressCB {
        let me = self.clone();
        Arc::new(move |live, _queued| me.forward(live))
    }

    fn forward(&self, live: Vec<ProgressView>) {
        {
            let mut last = self.mu.lock().unwrap();
            if last.elapsed() < THROTTLE_INTERVAL {
                return;
            }
            *last = Instant::now();
        }
        for v in live {
            self.bc.broadcast(Event {
                method: "onDownloadProgress".to_string(),
                params: snapshot_params(&v),
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn view() -> ProgressView {
        ProgressView {
            id: "odm-1".into(),
            url: "http://x".into(),
            final_url: "http://x".into(),
            filename: "x.bin".into(),
            state: download::TaskState::Queued,
            total_size: 0,
            bytes_done: 0,
            speed: 0,
            connections: 1,
            eta: Duration::ZERO,
            errors: 0,
            retries: 0,
            single_stream: false,
        }
    }

    #[tokio::test]
    async fn forwards_at_most_once_per_interval() {
        let bc = Broadcaster::new();
        let (_id, mut rx) = bc.register();
        let th = ProgressThrottler::new(bc.clone());
        th.forward(vec![view()]);
        assert_eq!(rx.recv().await.unwrap().method, "onDownloadProgress");
        // Immediate second call is swallowed by the throttle.
        th.forward(vec![view()]);
        assert!(rx.try_recv().is_err(), "second Forward must be throttled");
        // Params carry the snapshot shape.
        let bc2 = Broadcaster::new();
        let (_id2, mut rx2) = bc2.register();
        let v = view();
        bc2.broadcast(Event { method: "onDownloadProgress".into(), params: snapshot_params(&v) });
        let ev = rx2.recv().await.unwrap();
        assert_eq!(ev.params["goid"], json!("odm-1"));
        assert_eq!(ev.params["status"], json!("queued"));
        drop(th);
        let _ = _id;
    }
}
