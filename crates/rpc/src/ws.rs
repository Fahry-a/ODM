//! Rust port of ODM's `internal/rpc/ws.go` — the WebSocket event fan-out.
//! Each subscriber gets a bounded channel; a client whose buffer overflows
//! loses that one event (the engine is never backpressured by a slow GUI).

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::extract::ws::{Message, WebSocket};
use bytes::Bytes;
use futures_util::{SinkExt, StreamExt};
use tokio::sync::mpsc::{self, Receiver, Sender};
use tokio::sync::mpsc::error::TrySendError;
use tokio::time::{sleep_until, Instant, MissedTickBehavior};

use crate::types::Event;

/// Buffered events per subscriber (Go: `make(chan Event, 64)`).
const WS_SUB_BUFFER: usize = 64;
/// 60s pong wait × 9/10 (Go: `wsPingPeriod`).
const WS_PING_PERIOD: Duration = Duration::from_secs(54);
/// Drop clients that send nothing for 60s (Go's `pongWait` read deadline).
const WS_PONG_TIMEOUT: Duration = Duration::from_secs(60);

/// `Broadcaster` fans WebSocket events out to all connected subscribers.
pub struct Broadcaster {
    subs: Mutex<HashMap<u64, Sender<Event>>>,
    next_id: AtomicU64,
}

impl Broadcaster {
    pub fn new() -> Arc<Broadcaster> {
        Arc::new(Broadcaster { subs: Mutex::new(HashMap::new()), next_id: AtomicU64::new(1) })
    }

    /// `register` — attaches a new subscriber and returns its id plus the
    /// receiving half the pump writes into.
    pub fn register(&self) -> (u64, Receiver<Event>) {
        let id = self.next_id.fetch_add(1, Ordering::SeqCst);
        let (tx, rx) = mpsc::channel(WS_SUB_BUFFER);
        self.subs.lock().unwrap().insert(id, tx);
        (id, rx)
    }

    /// `unregister` — detaches a subscriber when its socket closes.
    pub fn unregister(&self, id: u64) {
        self.subs.lock().unwrap().remove(&id);
    }

    /// `broadcast` — non-blocking fan-out. A full buffer drops this event for
    /// that subscriber only (Go's `select { case ch <- e: default: }`); a closed
    /// channel means the subscriber is gone and is pruned.
    pub fn broadcast(&self, e: Event) {
        let mut subs = self.subs.lock().unwrap();
        let closed: Vec<u64> = subs
            .iter()
            .filter_map(|(id, tx)| match tx.try_send(e.clone()) {
                Ok(()) | Err(TrySendError::Full(_)) => None,
                Err(TrySendError::Closed(_)) => Some(*id),
            })
            .collect();
        for id in closed {
            subs.remove(&id);
        }
    }

    /// `subscriber_count` — live subscriber count (tests poll this after a
    /// `/ws` dial because registration is async).
    pub fn subscriber_count(&self) -> usize {
        self.subs.lock().unwrap().len()
    }

    /// `addWS` pump: forwards queued events to the socket, sends keepalive
    /// pings, and drives the socket's read half (so pong/close frames are
    /// processed). Ends on either side closing; unregisters on the way out.
    pub async fn pump(self: &Arc<Self>, socket: WebSocket, mut rx: Receiver<Event>, id: u64) {
        let (mut sink, mut stream) = socket.split();
        let mut ticker = tokio::time::interval(WS_PING_PERIOD);
        ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
        ticker.tick().await; // first tick is immediate — skip it
        let mut pong_deadline = Instant::now() + WS_PONG_TIMEOUT;

        loop {
            tokio::select! {
                msg = stream.next() => match msg {
                    Some(Ok(Message::Close(_))) | Some(Err(_)) | None => break,
                    // Any client frame proves liveness; Go resets the read
                    // deadline on each successful read, not just on pongs.
                    Some(Ok(_)) => pong_deadline = Instant::now() + WS_PONG_TIMEOUT,
                },
                ev = rx.recv() => match ev {
                    Some(ev) => {
                        let text = serde_json::to_string(&ev).unwrap_or_default();
                        if sink.send(Message::Text(text.into())).await.is_err() {
                            break;
                        }
                    },
                    None => break,
                },
                _ = ticker.tick() => {
                    if sink.send(Message::Ping(Bytes::from_static(b"odm"))).await.is_err() {
                        break;
                    }
                },
                _ = sleep_until(pong_deadline) => break,
                else => break,
            }
        }
        self.unregister(id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn broadcast_without_subscribers_is_a_noop() {
        let bc = Broadcaster::new();
        assert_eq!(bc.subscriber_count(), 0);
        bc.broadcast(Event { method: "onDownloadStart".into(), params: json!("x") });
        assert_eq!(bc.subscriber_count(), 0);
    }

    #[tokio::test]
    async fn broadcast_reaches_registered_subscriber() {
        let bc = Broadcaster::new();
        let (id, mut rx) = bc.register();
        assert_eq!(bc.subscriber_count(), 1);
        bc.broadcast(Event { method: "onDownloadStart".into(), params: json!({"goid": "odm-1"}) });
        let ev = rx.recv().await.unwrap();
        assert_eq!(ev.method, "onDownloadStart");
        assert_eq!(ev.params["goid"], json!("odm-1"));
        bc.unregister(id);
        assert_eq!(bc.subscriber_count(), 0);
    }

    #[tokio::test]
    async fn slow_subscriber_drops_event_but_stays_registered() {
        let bc = Broadcaster::new();
        let (id, _rx) = bc.register();
        for i in 0..(WS_SUB_BUFFER + 4) {
            bc.broadcast(Event { method: format!("onTick{i}"), params: json!(null) });
        }
        // The receiver is still alive, so the subscriber must not be pruned
        // even though its buffer overflowed.
        assert_eq!(bc.subscriber_count(), 1);
        bc.unregister(id);
    }
}
