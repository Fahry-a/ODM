//! Rust port of ODM's `internal/rpc/server_test.go` — live JSON-RPC + WS
//! plumbing tests over a real axum server (no stubbed dispatch).

use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::http::StatusCode;
use axum::Router;
use download::{Ctx, ExecOptions, Manager, TaskState};
use futures_util::StreamExt;
use rpc::{Broadcaster, Event, ProgressThrottler, Server, CODE_AUTH_ERROR, CODE_METHOD_NOT_FOUND};
use scheduler::{new_daemon, Scheduler};
use serde_json::{json, Value};

/// Keeps the daemon's ctx alive until the test ends, then signals the
/// scheduler to wind down. Without this, the daemon's `spawn_blocking(Wg::wait)`
/// never returns and the tokio runtime hangs on shutdown (Go's tests call
/// `defer cancel()` for the same reason).
struct CtxGuard {
    srv: Arc<Server>,
    _ctx: Ctx,
}

impl Drop for CtxGuard {
    fn drop(&mut self) {
        self.srv.daemon().signal_stop();
        let deadline = Instant::now() + Duration::from_secs(3);
        while !self.srv.daemon().dead() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
    }
}

/// Mirrors Go's `startServer`: a Manager + empty daemon scheduler + shared
/// Broadcaster feeding both the progress throttler and the Server.
fn start_server(secret: &str) -> (Arc<Server>, CtxGuard) {
    let mgr = Arc::new(
        Manager::new_manager(
            ExecOptions {
                dir: std::env::temp_dir().to_string_lossy().into_owned(),
                connections: 1,
                chunk_size: 8 * 1024,
                retry: 1,
                retry_wait: Duration::from_millis(5),
                timeout: Duration::from_secs(10),
                check_cert: true,
                ..Default::default()
            },
            None,
        )
        .unwrap(),
    );
    let bc = Broadcaster::new();
    let throttler = ProgressThrottler::new(bc.clone());
    let maker = {
        let mgr = mgr.clone();
        Arc::new(move |url: &str, _idx: i64| mgr.new_task(url))
    };
    let sch = Scheduler::new_empty_scheduler(2, maker, Some(throttler.callback()));
    let d = new_daemon(sch, mgr);
    let ctx = Ctx::new();
    d.start(&ctx);
    let srv = Server::new_with_broadcaster(d, secret, bc);
    let srv2 = srv.clone();
    srv.daemon().on_complete(Arc::new(move |v| srv2.on_task_complete(v)));
    (srv.clone(), CtxGuard { srv, _ctx: ctx })
}

async fn spawn_http(srv: Arc<Server>) -> (String, tokio::task::JoinHandle<()>) {
    let app = srv.routes();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    (format!("http://{addr}"), task)
}

async fn rpc(base: &str, method: &str, params: Value, id: i64) -> Value {
    let client = reqwest::Client::new();
    let resp = client
        .post(format!("{base}/rpc"))
        .json(&json!({"jsonrpc": "2.0", "method": method, "params": params, "id": id}))
        .send()
        .await
        .unwrap();
    resp.json::<Value>().await.unwrap()
}

async fn wait_subscribers(srv: &Server, n: usize) {
    let deadline = Instant::now() + Duration::from_secs(3);
    while srv.broadcaster().subscriber_count() < n && Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(srv.broadcaster().subscriber_count() >= n, "subscriber never registered on /ws (want {n})");
}

async fn wait_dead(srv: &Server, secs: u64) -> bool {
    let deadline = Instant::now() + Duration::from_secs(secs);
    while Instant::now() < deadline {
        if srv.daemon().dead() {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    srv.daemon().dead()
}

#[tokio::test(flavor = "multi_thread")]
async fn auth_enforced() {
    let (srv, _ctx) = start_server("hunter2");
    let (base, _h) = spawn_http(srv).await;
    let r = rpc(&base, "odm.getVersion", json!([]), 1).await;
    assert_eq!(r["error"]["code"], json!(CODE_AUTH_ERROR));
    let r = rpc(&base, "odm.getVersion", json!(["token:hunter2"]), 1).await;
    assert!(r.get("error").is_none(), "good token should pass: {r}");
    let r = rpc(&base, "odm.getVersion", json!(["token:wrong"]), 1).await;
    assert_eq!(r["error"]["code"], json!(CODE_AUTH_ERROR));
}

#[tokio::test(flavor = "multi_thread")]
async fn get_version_and_unknown_method_over_http() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv).await;
    let r = rpc(&base, "odm.getVersion", json!([]), 1).await;
    assert!(r["result"]["version"].as_str().unwrap().starts_with("odm/"));
    assert!(!r["result"]["enabledFeatures"].as_array().unwrap().is_empty());
    let r = rpc(&base, "odm.bogus", json!([]), 2).await;
    assert_eq!(r["error"]["code"], json!(CODE_METHOD_NOT_FOUND));
    let r = rpc(&base, "odm.getGlobalStat", json!([]), 3).await;
    assert!(r["result"].get("active").is_some());
}

#[tokio::test(flavor = "multi_thread")]
async fn get_version_advertises_tls_only_when_enabled() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv.clone()).await;
    let r = rpc(&base, "odm.getVersion", json!([]), 1).await;
    let features = r["result"]["enabledFeatures"].as_array().unwrap();
    assert!(!features.iter().any(|f| f == "TLS"), "TLS advertised while serving plain HTTP: {features:?}");
    srv.set_tls(true);
    let r = rpc(&base, "odm.getVersion", json!([]), 2).await;
    let features = r["result"]["enabledFeatures"].as_array().unwrap();
    assert!(features.iter().any(|f| f == "TLS"), "TLS missing after enabling: {features:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn add_uri_appears_in_tell_lists() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv).await;
    let r = rpc(&base, "odm.addUri", json!(["http://example.invalid/file.bin", 2]), 1).await;
    assert!(r.get("error").is_none(), "addUri error: {r}");
    let gid = r["result"].as_str().unwrap().to_string();
    assert!(!gid.is_empty());

    let deadline = Instant::now() + Duration::from_secs(2);
    let mut found = false;
    while Instant::now() < deadline {
        for method in ["odm.tellActive", "odm.tellWaiting", "odm.tellStopped"] {
            let r = rpc(&base, method, json!([]), 2).await;
            if r["result"].as_array().map_or(false, |a| !a.is_empty()) {
                found = true;
                break;
            }
        }
        if found {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert!(found, "added task never appeared in tellActive/tellWaiting/tellStopped");
}

#[tokio::test(flavor = "multi_thread")]
async fn change_option_rejects_zero_and_negative_connections() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv).await;
    let r = rpc(&base, "odm.addUri", json!(["http://example.invalid/file.bin", 2]), 1).await;
    let gid = r["result"].as_str().unwrap();
    for nc in [0, -3] {
        let r = rpc(&base, "odm.changeOption", json!([gid, {"connections": nc}]), 2).await;
        assert!(r.get("error").is_some(), "connections={nc} must be rejected: {r}");
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn shutdown_responds_then_stops_and_is_auth_gated() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv.clone()).await;
    let r = rpc(&base, "odm.shutdown", json!([]), 1).await;
    assert_eq!(r["result"], json!("OK"));
    assert!(wait_dead(&srv, 3).await, "daemon did not stop after odm.shutdown");

    // With a secret, an unauthenticated shutdown must be rejected AND the
    // daemon must stay alive.
    let (srv2, _ctx2) = start_server("topsecret");
    let (base2, _h2) = spawn_http(srv2.clone()).await;
    let r = rpc(&base2, "odm.shutdown", json!([]), 1).await;
    assert_eq!(r["error"]["code"], json!(CODE_AUTH_ERROR));
    assert!(!wait_dead(&srv2, 0).await, "daemon stopped despite unauthenticated shutdown");
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(!srv2.daemon().dead(), "daemon stopped despite unauthenticated shutdown");
}

#[tokio::test(flavor = "multi_thread")]
async fn add_uri_rejected_after_shutdown() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv.clone()).await;
    let r = rpc(&base, "odm.shutdown", json!([]), 1).await;
    assert!(r.get("error").is_none());
    assert!(wait_dead(&srv, 3).await);
    let r = rpc(&base, "odm.addUri", json!(["http://example.invalid/late.bin", 1]), 2).await;
    assert!(r.get("error").is_some(), "addUri after shutdown must fail: {r}");
}

#[tokio::test(flavor = "multi_thread")]
async fn ws_dial_event_and_secret() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv.clone()).await;
    let ws_url = base.replacen("http", "ws", 1) + "/ws";
    let (mut ws, _) = tokio_tungstenite::connect_async(&ws_url).await.unwrap();
    wait_subscribers(&srv, 1).await;

    let r = rpc(&base, "odm.addUri", json!(["http://example.invalid/ws-event.bin", 1]), 1).await;
    let gid = r["result"].as_str().unwrap().to_string();

    let deadline = Instant::now() + Duration::from_secs(5);
    let mut seen = false;
    while Instant::now() < deadline {
        match tokio::time::timeout(Duration::from_secs(2), ws.next()).await {
            Ok(Some(Ok(msg))) if msg.is_text() => {
                let ev: Event = serde_json::from_slice(&msg.into_data()).unwrap();
                if ev.method == "onDownloadStart" {
                    assert_eq!(ev.params["goid"], json!(gid));
                    seen = true;
                    break;
                }
            }
            Ok(Some(Err(_))) | Ok(None) | Err(_) => break,
            _ => {}
        }
    }
    assert!(seen, "no onDownloadStart event over /ws");

    // Secret gating: no secret → 401, correct ?secret → upgrade.
    let (srv2, _ctx2) = start_server("topsecret");
    let (base2, _h2) = spawn_http(srv2).await;
    let ws2 = base2.replacen("http", "ws", 1) + "/ws";
    assert!(tokio_tungstenite::connect_async(&ws2).await.is_err(), "missing secret must fail");
    let ws3 = ws2 + "?secret=topsecret";
    assert!(tokio_tungstenite::connect_async(&ws3).await.is_ok(), "correct secret must upgrade");
}

/// A terminal snapshot used to exercise `Server::on_task_complete`'s mapping.
fn terminal_view(state: TaskState) -> download::ProgressView {
    download::ProgressView {
        id: "odm-1".into(),
        url: "http://example.invalid/x.bin".into(),
        final_url: "http://example.invalid/x.bin".into(),
        filename: "x.bin".into(),
        state,
        total_size: 1024,
        bytes_done: 1024,
        speed: 0,
        connections: 1,
        eta: Duration::ZERO,
        errors: 0,
        retries: 0,
        single_stream: false,
    }
}

/// `onDownloadComplete` mapping from a terminal snapshot (the remaining leg of
/// the completion path — onDownloadStart is engine-driven in
/// `ws_dial_event_and_secret`, onDownloadError in `ws_error_event_for_failing_task`).
#[tokio::test(flavor = "multi_thread")]
async fn ws_completion_event_from_task_complete() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv.clone()).await;
    let ws_url = base.replacen("http", "ws", 1) + "/ws";
    let (mut ws, _) = tokio_tungstenite::connect_async(&ws_url).await.unwrap();
    wait_subscribers(&srv, 1).await;

    srv.on_task_complete(terminal_view(TaskState::Completed));

    let mut seen = std::collections::HashSet::new();
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        match tokio::time::timeout(Duration::from_secs(2), ws.next()).await {
            Ok(Some(Ok(msg))) if msg.is_text() => {
                let ev: Event = serde_json::from_slice(&msg.into_data()).unwrap();
                seen.insert(ev.method.clone());
                if seen.contains("onDownloadComplete") {
                    break;
                }
            }
            Ok(Some(Err(_))) | Ok(None) | Err(_) => break,
            _ => {}
        }
    }
    assert!(seen.contains("onDownloadComplete"), "missing onDownloadComplete: {seen:?}");
    assert!(!seen.contains("onDownloadError"), "unexpected onDownloadError: {seen:?}");

    // A non-completed terminal state maps to the error event instead.
    srv.on_task_complete(terminal_view(TaskState::Error));
    let mut errored = false;
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        match tokio::time::timeout(Duration::from_secs(2), ws.next()).await {
            Ok(Some(Ok(msg))) if msg.is_text() => {
                let ev: Event = serde_json::from_slice(&msg.into_data()).unwrap();
                if ev.method == "onDownloadError" {
                    errored = true;
                    break;
                }
            }
            Ok(Some(Err(_))) | Ok(None) | Err(_) => break,
            _ => {}
        }
    }
    assert!(errored, "non-completed terminal state must emit onDownloadError");
}

#[tokio::test(flavor = "multi_thread")]
async fn ws_error_event_for_failing_task() {
    let (srv, _ctx) = start_server("");
    let (base, _h) = spawn_http(srv.clone()).await;
    // A 500-returning payload server.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let _ph = tokio::spawn(async move {
        let app = Router::new().route(
            "/file.bin",
            axum::routing::any(|| async { StatusCode::INTERNAL_SERVER_ERROR }),
        );
        let _ = axum::serve(listener, app).await;
    });
    let ws_url = base.replacen("http", "ws", 1) + "/ws";
    let (mut ws, _) = tokio_tungstenite::connect_async(&ws_url).await.unwrap();
    wait_subscribers(&srv, 1).await;
    let r = rpc(&base, "odm.addUri", json!([format!("http://{addr}/file.bin"), 1]), 1).await;
    assert!(r.get("error").is_none());

    let mut seen = std::collections::HashSet::new();
    let deadline = Instant::now() + Duration::from_secs(30);
    while Instant::now() < deadline {
        match tokio::time::timeout(Duration::from_secs(2), ws.next()).await {
            Ok(Some(Ok(msg))) if msg.is_text() => {
                let ev: Event = serde_json::from_slice(&msg.into_data()).unwrap();
                seen.insert(ev.method.clone());
                if seen.contains("onDownloadStart") && seen.contains("onDownloadError") {
                    break;
                }
            }
            Ok(Some(Err(_))) | Ok(None) | Err(_) => break,
            _ => {}
        }
    }
    assert!(seen.contains("onDownloadStart"), "missing onDownloadStart: {seen:?}");
    assert!(seen.contains("onDownloadError"), "missing onDownloadError: {seen:?}");
}
