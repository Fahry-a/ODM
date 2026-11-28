//! Rust port of ODM's `internal/rpc/server.go` — the JSON-RPC 2.0 dispatcher,
//! HTTP routes, auth, response shaping and the WebSocket upgrade handler.

use std::collections::HashMap;
use std::sync::Arc;

use axum::body::to_bytes;
use axum::extract::ws::WebSocketUpgrade;
use axum::extract::{Query, Request, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use download::{ProgressView, TaskID};
use scheduler::Daemon;
use serde_json::{json, Value};
use version::VERSION;

use crate::types::*;
use crate::ws::Broadcaster;

const BODY_LIMIT: usize = 1 << 20;
const WS_READ_LIMIT: usize = 1 << 20;

/// `Server` — the JSON-RPC + WebSocket surface over a `scheduler::Daemon`.
pub struct Server {
    daemon: Arc<Daemon>,
    bc: Arc<Broadcaster>,
    secret: String,
}

impl Server {
    /// `NewServer` — allocates its own Broadcaster.
    pub fn new(daemon: Arc<Daemon>, secret: impl Into<String>) -> Arc<Server> {
        Self::new_with_broadcaster(daemon, secret, Broadcaster::new())
    }

    /// `NewServerWithBroadcaster` — adopts a shared Broadcaster so the
    /// scheduler's progress callback and the server's own emissions reach the
    /// same subscribers (mirrors `runRPC`).
    pub fn new_with_broadcaster(
        daemon: Arc<Daemon>,
        secret: impl Into<String>,
        bc: Arc<Broadcaster>,
    ) -> Arc<Server> {
        Arc::new(Server { daemon, bc, secret: secret.into() })
    }

    /// `OnTaskComplete` — maps a terminal snapshot onto the matching event.
    pub fn on_task_complete(&self, v: ProgressView) {
        let method = if v.state.as_str() == "completed" {
            "onDownloadComplete"
        } else {
            // Any non-completed terminal state (error/cancelled) is an error.
            "onDownloadError"
        };
        self.bc.broadcast(Event { method: method.to_string(), params: db_snapshot(&v) });
    }

    pub fn broadcaster(&self) -> Arc<Broadcaster> {
        self.bc.clone()
    }

    pub fn daemon(&self) -> Arc<Daemon> {
        self.daemon.clone()
    }

    /// `Routes` — installs `/rpc` and `/ws`; the caller binds the address.
    pub fn routes(self: &Arc<Self>) -> Router {
        Router::new()
            .route("/rpc", post(Self::handle_rpc))
            .route("/ws", get(Self::handle_ws))
            .with_state(self.clone())
    }

    // --- auth --------------------------------------------------------------

    /// `checkSecret` — aria2-style `token:<secret>` first-param auth. Empty
    /// configured secret disables auth. Uses a constant-time byte compare so a
    /// network-exposed daemon (--rpc-listen-all) doesn't leak the secret one
    /// byte at a time through response timing.
    fn check_secret(&self, req: &JsonRpcRequest) -> bool {
        if self.secret.is_empty() {
            return true;
        }
        match req.params.first() {
            Some(Value::String(first)) => {
                first.starts_with("token:") && constant_time_eq(&first.as_bytes()[6..], self.secret.as_bytes())
            }
            _ => false,
        }
    }

    /// `checkQuerySecret` — `?secret=<value>` auth for browser `/ws` clients
    /// (the framed token doesn't fit a direct ws:// URL). Constant-time too.
    fn check_query_secret(&self, q: &HashMap<String, String>) -> bool {
        if self.secret.is_empty() {
            return true;
        }
        constant_time_eq(q.get("secret").map(String::as_str).unwrap_or("").as_bytes(), self.secret.as_bytes())
    }

    // --- HTTP dispatch -----------------------------------------------------

    async fn handle_ws(
        State(srv): State<Arc<Server>>,
        Query(q): Query<HashMap<String, String>>,
        ws: WebSocketUpgrade,
    ) -> Response {
        if !srv.check_query_secret(&q) {
            return (StatusCode::UNAUTHORIZED, "unauthorized").into_response();
        }
        let bc = srv.bc.clone();
        let (id, rx) = bc.register();
        ws.max_message_size(WS_READ_LIMIT)
            .on_upgrade(move |socket| async move { bc.pump(socket, rx, id).await })
    }

    async fn handle_rpc(State(srv): State<Arc<Server>>, req: Request) -> Response {
        let body = match to_bytes(req.into_body(), BODY_LIMIT).await {
            Ok(b) => b,
            Err(e) => {
                return Json(JsonRpcResponse::err(Value::Null, CODE_PARSE_ERROR, format!("parse: {e}")))
                    .into_response()
            }
        };
        let trimmed = String::from_utf8_lossy(&body).trim().to_string();

        // JSON-RPC batches are arrays; support both single and batch requests.
        if trimmed.starts_with('[') {
            let batch: Vec<JsonRpcRequest> = match serde_json::from_slice(&body) {
                Ok(v) => v,
                Err(e) => {
                    return Json(JsonRpcResponse::err(Value::Null, CODE_PARSE_ERROR, format!("batch parse: {e}")))
                        .into_response()
                }
            };
            // Gate shutdown on auth: dispatch is where checkSecret runs, so an
            // unauthenticated odm.shutdown leaves the daemon running.
            let shutdown = batch.iter().any(|q| q.method == "odm.shutdown" && srv.check_secret(q));
            let responses: Vec<JsonRpcResponse> = batch.iter().map(|q| srv.dispatch(q)).collect();
            if shutdown {
                srv.daemon.signal_stop();
            }
            return Json(responses).into_response();
        }

        let req: JsonRpcRequest = match serde_json::from_slice(&body) {
            Ok(v) => v,
            Err(e) => {
                return Json(JsonRpcResponse::err(Value::Null, CODE_PARSE_ERROR, format!("parse: {e}")))
                    .into_response()
            }
        };
        let shutdown = req.method == "odm.shutdown" && srv.check_secret(&req);
        let resp = srv.dispatch(&req);
        if shutdown {
            srv.daemon.signal_stop();
        }
        Json(resp).into_response()
    }

    /// `dispatch` — routes one request through the method table.
    fn dispatch(&self, req: &JsonRpcRequest) -> JsonRpcResponse {
        let id = req.id.clone();
        if !self.check_secret(req) {
            return JsonRpcResponse::err(id, CODE_AUTH_ERROR, "bad or missing secret");
        }
        match req.method.as_str() {
            "odm.addUri" => self.method_add_uri(req, id),
            "odm.addBatch" => self.method_add_batch(req, id),
            "odm.pause" => self.method_pause(req, id, false),
            "odm.pauseAll" => self.method_pause(req, id, true),
            "odm.unpause" => self.method_unpause(req, id, false),
            "odm.unpauseAll" => self.method_unpause(req, id, true),
            "odm.remove" => self.method_remove(req, id),
            "odm.tellStatus" => self.method_tell_status(req, id),
            "odm.tellActive" => JsonRpcResponse::ok(id, json!(db_snapshots(&self.daemon.tell_active()))),
            "odm.tellWaiting" => JsonRpcResponse::ok(id, json!(db_snapshots(&self.daemon.tell_waiting()))),
            "odm.tellStopped" => JsonRpcResponse::ok(id, json!(db_snapshots(&self.daemon.tell_stopped()))),
            "odm.changeOption" => self.method_change_option(req, id),
            "odm.getGlobalStat" => JsonRpcResponse::ok(
                id,
                json!({
                    "active": self.daemon.tell_active().len(),
                    "waiting": self.daemon.tell_waiting().len(),
                    "stopped": self.daemon.tell_stopped().len(),
                }),
            ),
            "odm.getVersion" => {
                let features = ["Multi-Connection", "Range-Download", "Resume", "Batch", "Checksum", "RateLimit-Agent", "TLS"];
                JsonRpcResponse::ok(id, json!({ "version": VERSION, "enabledFeatures": features }))
            },
            // The real stop happens in handle_rpc after the reply is built;
            // reaching dispatch here only produces the OK response.
            "odm.shutdown" | "odm.noop" => JsonRpcResponse::ok(id, json!("OK")),
            _ => JsonRpcResponse::err(id, CODE_METHOD_NOT_FOUND, format!("method not found: {}", req.method)),
        }
    }

    // --- parameter helpers -------------------------------------------------

    fn method_add_uri(&self, req: &JsonRpcRequest, id: Value) -> JsonRpcResponse {
        let p = strip_token(&req.params);
        let url = match str_param(p, 0) {
            Ok(v) => v,
            Err(e) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, e),
        };
        let conns = int_param(p, 1).unwrap_or(0) as i32;
        let task_id = match self.daemon.add_url(&url, conns) {
            Ok(v) => v,
            Err(e) => return JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, e.msg.clone()),
        };
        self.bc.broadcast(Event {
            method: "onDownloadStart".to_string(),
            params: json!({"goid": task_id}),
        });
        JsonRpcResponse::ok(id, json!(task_id))
    }

    fn method_add_batch(&self, req: &JsonRpcRequest, id: Value) -> JsonRpcResponse {
        let p = strip_token(&req.params);
        let urls = match p.first() {
            Some(Value::Array(v)) => v,
            Some(_) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "params[0] must be a url list"),
            None => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "no urls"),
        };
        let conns = int_param(p, 1).unwrap_or(0) as i32;
        let mut ids = Vec::new();
        for u in urls {
            if let Value::String(url) = u {
                if let Ok(tid) = self.daemon.add_url(url, conns) {
                    ids.push(tid);
                }
            }
        }
        JsonRpcResponse::ok(id, json!(ids))
    }

    fn method_pause(&self, req: &JsonRpcRequest, id: Value, all: bool) -> JsonRpcResponse {
        if all {
            for v in self.daemon.tell_active() {
                self.daemon.pause(&v.id);
            }
            return JsonRpcResponse::ok(id, json!("OK"));
        }
        let p = strip_token(&req.params);
        let gid = match str_param(p, 0) {
            Ok(v) => v,
            Err(e) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, e),
        };
        if self.daemon.pause(&gid) {
            self.bc.broadcast(Event {
                method: "onDownloadPause".to_string(),
                params: json!({"goid": gid}),
            });
            JsonRpcResponse::ok(id, json!("OK"))
        } else {
            JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, "task not found")
        }
    }

    fn method_unpause(&self, req: &JsonRpcRequest, id: Value, all: bool) -> JsonRpcResponse {
        if all {
            for v in self.daemon.tell_active() {
                self.daemon.unpause(&v.id);
            }
            return JsonRpcResponse::ok(id, json!("OK"));
        }
        let p = strip_token(&req.params);
        let gid = match str_param(p, 0) {
            Ok(v) => v,
            Err(e) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, e),
        };
        if self.daemon.unpause(&gid) {
            JsonRpcResponse::ok(id, json!("OK"))
        } else {
            JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, "task not found")
        }
    }

    fn method_remove(&self, req: &JsonRpcRequest, id: Value) -> JsonRpcResponse {
        let p = strip_token(&req.params);
        let gid = match str_param(p, 0) {
            Ok(v) => v,
            Err(e) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, e),
        };
        if self.daemon.remove(&gid) {
            JsonRpcResponse::ok(id, json!("OK"))
        } else {
            JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, "task not found")
        }
    }

    fn method_tell_status(&self, req: &JsonRpcRequest, id: Value) -> JsonRpcResponse {
        let p = strip_token(&req.params);
        let gid = match str_param(p, 0) {
            Ok(v) => v,
            Err(e) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, e),
        };
        match self.daemon.tell_status(&gid) {
            Some(v) => JsonRpcResponse::ok(id, db_snapshot(&v)),
            None => JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, "task not found"),
        }
    }

    fn method_change_option(&self, req: &JsonRpcRequest, id: Value) -> JsonRpcResponse {
        let p = strip_token(&req.params);
        if p.len() < 2 {
            return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "changeOption needs gid and options dict");
        }
        let gid = match p[0].as_str() {
            Some(v) => v.to_string(),
            None => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "gid must be a string"),
        };
        let opts = match p[1].as_object() {
            Some(v) => v,
            None => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "options must be a dict"),
        };
        for (k, v) in opts {
            match k.as_str() {
                "max-download-limit" => {
                    let val = match v.as_str() {
                        Some(s) => s,
                        None => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "max-download-limit must be a string"),
                    };
                    if let Err(e) = self.daemon.change_global_limit(val) {
                        return JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, e);
                    }
                }
                "max-download-limit-per-task" => {
                    let val = match v.as_str() {
                        Some(s) => s,
                        None => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "max-download-limit-per-task must be a string"),
                    };
                    if !self.daemon.change_task_limit(&gid, val) {
                        return JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, "task not found");
                    }
                }
                "connections" => {
                    let nc = match int_param(std::slice::from_ref(v), 0) {
                        Ok(n) => n,
                        Err(_) => return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "connections must be an integer"),
                    };
                    // 0/negative would retire every worker via graceful drain
                    // while chunks remain, reporting a half-empty file as
                    // complete with its control file deleted. Reject here.
                    if nc < 1 {
                        return JsonRpcResponse::err(id, CODE_INVALID_PARAMS, "connections must be at least 1");
                    }
                    if !self.daemon.change_conns(&TaskID::from(gid.clone()), nc as i32) {
                        return JsonRpcResponse::err(id, CODE_INTERNAL_ERROR, "task not found (or not started yet)");
                    }
                }
                _ => {}
            }
        }
        JsonRpcResponse::ok(id, json!("OK"))
    }
}

/// `stripToken` — drops a leading `token:...` first param (auth already passed
/// in dispatch). Ordinary string args starting with something else are kept.
fn strip_token(p: &[Value]) -> &[Value] {
    match p.first() {
        Some(Value::String(s)) if s.starts_with("token:") => &p[1..],
        _ => p,
    }
}

fn str_param(p: &[Value], idx: usize) -> Result<String, String> {
    match p.get(idx) {
        Some(Value::String(s)) => Ok(s.clone()),
        Some(_) => Err(format!("param {idx} not a string")),
        None => Err(format!("missing param {idx}")),
    }
}

fn int_param(p: &[Value], idx: usize) -> Result<i64, String> {
    match p.get(idx) {
        Some(Value::Number(n)) => n
            .as_i64()
            .or_else(|| n.as_f64().map(|f| f as i64))
            .ok_or_else(|| format!("param {idx} not a number")),
        Some(Value::String(s)) => s.parse::<i64>().map_err(|_| format!("not an integer: {s:?}")),
        Some(_) => Err(format!("param {idx} not a number")),
        None => Err(format!("missing param {idx}")),
    }
}

/// Go's `crypto/subtle.ConstantTimeCompare`: length mismatch fails fast, then
/// every byte is accumulated without early exit so timing doesn't leak the
/// secret one byte at a time.
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b) {
        diff |= x ^ y;
    }
    diff == 0
}

/// `dbSnapshot` — internal ProgressView → the JSON shape GUIs expect (aria2
/// `tellStatus`-flavoured field names).
fn db_snapshot(v: &ProgressView) -> Value {
    json!({
        "goid": v.id,
        "url": v.url,
        "finalUrl": v.final_url,
        "filename": v.filename,
        "status": v.state.as_str(),
        "totalSize": v.total_size,
        "bytesDone": v.bytes_done,
        "speed": v.speed,
        "connections": v.connections,
        "etaSeconds": v.eta.as_secs() as i64,
        "errors": v.errors,
        "retries": v.retries,
        "singleStream": v.single_stream,
    })
}

fn db_snapshots(vs: &[ProgressView]) -> Vec<Value> {
    vs.iter().map(db_snapshot).collect()
}

/// `SnapshotParams` — exported form of dbSnapshot for the progress throttler.
pub fn snapshot_params(v: &ProgressView) -> Value {
    db_snapshot(v)
}
