//! Go is the primary download core: the Rust CLI dispatches one-shot
//! downloads to the embedded Go engine (static c-archive). ODM_RUST_ENGINE=1
//! selects the Rust engine as an A/B fallback; --rpc always stays on Rust.

use config::Options;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::ffi::{CStr, CString};
use std::fs::{File, OpenOptions};
use std::io::{IsTerminal, Write};
use std::os::raw::{c_char, c_int};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;
use tokio::sync::{mpsc, oneshot};

// Fixed ABI implemented by gocore/cmd/odmbridge (libodmgo.a).
extern "C" {
    fn odm_go_run(config_json: *const c_char, result_buf: *mut c_char, result_len: c_int) -> c_int;
    fn odm_go_set_progress_callback(cb: extern "C" fn(*const c_char));
    fn odm_go_cancel();
}

const RESULT_CAP: usize = 1 << 20;

/// Set by the signal handler before odm_go_cancel, so we can pick the
/// cancelled exit code/message after the synchronous Go run unwinds.
static CANCELLED: AtomicBool = AtomicBool::new(false);
static SESSION: Mutex<Option<File>> = Mutex::new(None);
/// The C progress callback has no userdata; share task snapshots and the UI
/// sender through statics (same pattern as SESSION).
type ViewMap = HashMap<String, download::ProgressView>;
static LIVE_TASKS: LazyLock<Mutex<ViewMap>> = LazyLock::new(|| Mutex::new(HashMap::new()));
static LIVE_TX: Mutex<Option<mpsc::Sender<Vec<download::ProgressView>>>> = Mutex::new(None);

/// Serialize the full CLI/config state into the bridge's config JSON.
/// Pure function so it can be unit-tested without Go linkage.
fn build_config_json(o: &Options) -> String {
    // The Go bridge joins Dir + OutFile; an absolute -o must be split first.
    let (out_file, dir) = if std::path::Path::new(&o.out_file).is_absolute() {
        let p = std::path::Path::new(&o.out_file);
        (
            p.file_name().unwrap_or_default().to_string_lossy().into_owned(),
            p.parent().unwrap_or_else(|| std::path::Path::new("")).to_string_lossy().into_owned(),
        )
    } else {
        (o.out_file.clone(), o.dir.clone())
    };
    json!({
        "urls": o.urls,
        "out_file": out_file,
        "dir": dir,
        "connections": o.connections,
        "max_connections": o.max_connection,
        "split_file": o.split_file,
        "chunk_size": o.chunk_size,
        "retry": o.retry,
        "retry_wait": o.retry_wait,
        "timeout": o.timeout,
        "max_redirect": o.max_redirect,
        "user_agent": o.user_agent,
        "headers": o.headers,
        "load_cookies": o.load_cookie,
        "referer": o.referer,
        "proxy": o.proxy,
        "check_certificate": o.check_certificate,
        "checksum": o.checksum,
        "checksum_url": o.checksum_url,
        "mirrors": o.mirrors,
        "limit_rate": o.limit_rate,
        "limit_rate_per_task": o.task_limit_rate,
        "auto_rename": o.auto_rename,
        "skip_existing": o.skip_existing,
        "continue_": o.continue_,
        "profile": o.profile,
        "split": o.split,
        "min_split_size": o.min_split_size,
        "max_connection_per_server": o.max_conn_per_server,
        "quiet": o.quiet,
    })
    .to_string()
}

/// Map the Go state string onto the renderer's TaskState.
fn state_from_str(s: &str) -> download::TaskState {
    match s {
        "active" | "downloading" => download::TaskState::Active,
        "completed" => download::TaskState::Completed,
        "error" => download::TaskState::Error,
        "paused" => download::TaskState::Paused,
        "queued" => download::TaskState::Queued,
        "retrying" => download::TaskState::Retrying,
        _ => download::TaskState::Active,
    }
}

/// Parse one Go progress event into the view the pacman renderer consumes.
fn progress_view(v: &Value) -> Option<download::ProgressView> {
    let id = v["id"].as_str()?.to_string();
    let url = v["url"].as_str().unwrap_or("").to_string();
    let state = state_from_str(v["state"].as_str().unwrap_or(""));
    let bytes = v["bytes"].as_i64().unwrap_or(0);
    let total = v["total"].as_i64().unwrap_or(0);
    // Go emits speed as a JSON float; as_i64 would drop fractional values.
    let speed = v["speed"].as_f64().unwrap_or(0.0) as i64;
    let eta = if speed > 0 && total > bytes {
        Duration::from_secs(((total - bytes) / speed) as u64)
    } else {
        Duration::ZERO
    };
    Some(download::ProgressView {
        id,
        final_url: url.clone(),
        url,
        filename: v["file"].as_str().unwrap_or("").to_string(),
        state,
        total_size: total,
        bytes_done: bytes,
        speed,
        connections: v["conns"].as_i64().unwrap_or(0) as i32,
        eta,
        errors: v["errors"].as_i64().unwrap_or(0) as i32,
        retries: v["retries"].as_i64().unwrap_or(0) as i32,
        single_stream: false,
    })
}

/// Progress events from Go: {event,id,url,file,state,bytes,total,speed,conns,errors,retries}.
/// Appends the raw event to the session log and feeds the pacman renderer a
/// full snapshot of every task seen so far (including finished ones, so the
/// final frame shows them). Never blocks: the C caller must not be stalled.
extern "C" fn on_progress(event_json: *const c_char) {
    if event_json.is_null() {
        return;
    }
    let raw = unsafe { CStr::from_ptr(event_json) }.to_string_lossy();

    if let Ok(mut g) = SESSION.lock() {
        if let Some(f) = g.as_mut() {
            let _ = writeln!(f, "{raw}");
        }
    }

    let v: Value = match serde_json::from_str(&raw) {
        Ok(v) => v,
        Err(_) => return,
    };
    let Some(view) = progress_view(&v) else {
        return;
    };

    let snapshot = {
        let Ok(mut tasks) = LIVE_TASKS.lock() else {
            return;
        };
        tasks.insert(view.id.clone(), view);
        tasks.values().cloned().collect::<Vec<_>>()
    };
    let Ok(tx) = LIVE_TX.lock() else { return };
    if let Some(tx) = tx.as_ref() {
        let _ = tx.try_send(snapshot);
    }
}

/// The CLI already runs inside a Tokio multi-thread runtime; a spawned task
/// keeps polling signals while the synchronous odm_go_run call blocks.
fn install_cancel_handler() {
    tokio::spawn(async {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{signal, SignalKind};
            if let Ok(mut term) = signal(SignalKind::terminate()) {
                tokio::select! {
                    _ = tokio::signal::ctrl_c() => {},
                    _ = term.recv() => {},
                }
            } else {
                let _ = tokio::signal::ctrl_c().await;
            }
        }
        #[cfg(not(unix))]
        let _ = tokio::signal::ctrl_c().await;
        CANCELLED.store(true, Ordering::SeqCst);
        unsafe { odm_go_cancel() };
    });
}

fn open_session(path: &str) -> Result<Option<File>, String> {
    if path.is_empty() {
        return Ok(None);
    }
    let mut opts = OpenOptions::new();
    opts.create(true).append(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600); // matches production's --session-log mode
    }
    let f = opts
        .open(path)
        .map_err(|e| format!("session-log {path:?}: {e}"))?;
    Ok(Some(f))
}

/// `--dry-run` stays a Rust-side CLI concern: probe with the Rust transport,
/// show the plan, and never invoke the Go engine.
async fn dry_run(o: &Options) -> i32 {
    println!("dry-run: probe + allocation only, nothing downloaded");
    let cfg = transport::ClientConfig {
        user_agent: o.user_agent.clone(),
        headers: o.headers.clone(),
        referer: o.referer.clone(),
        proxy: o.proxy.clone(),
        check_certificate: o.check_certificate,
        max_redirect: o.max_redirect as i32,
        timeout: Duration::from_secs(o.timeout.max(0) as u64),
        http2: false,
    };
    let Ok(client) = transport::new_client(cfg) else {
        eprintln!("odm: go engine: cannot build probe client");
        return 1;
    };
    let mut total = 0i64;
    let mut known = 0usize;
    for u in &o.urls {
        if let Ok(pr) = client.probe(u).await {
            if pr.total_size > 0 {
                total += pr.total_size;
                known += 1;
            }
        }
    }
    let size = if known > 0 { ui::format_file_size(total) } else { "unknown".to_string() };
    println!("would download {} file(s), total {}", o.urls.len(), size);
    println!("balancer mode {}: {} parallel", if o.urls.len() == 1 { "A" } else { "B" }, o.urls.len().min(o.connections.max(1) as usize));
    0
}

pub async fn run(o: &Options) -> i32 {
    if !o.rpc && o.urls.is_empty() {
        eprintln!("odm: no URLs provided (pass URLs as args, -i <file>, or use --rpc)");
        return download::exit_code::EXIT_GENERAL;
    }
    if o.dry_run {
        return dry_run(o).await;
    }
    match open_session(&o.session_log) {
        Ok(f) => *SESSION.lock().unwrap() = f,
        Err(e) => {
            eprintln!("odm: {e}");
            return 1;
        }
    }

    // Same renderer selection and snapshot channel layout as the Rust engine,
    // so both paths draw the identical pacman/ILoveCandy bars.
    let renderer = Arc::new(
        if std::io::stdout().is_terminal() || !std::io::stderr().is_terminal() {
            ui::Renderer::new_stdout(o.quiet)
        } else {
            ui::Renderer::new_stderr(o.quiet)
        },
    );
    let (live_tx, live_rx) = mpsc::channel::<Vec<download::ProgressView>>(16);
    let (_queued_tx, queued_rx) = mpsc::channel::<Vec<download::ProgressView>>(16);
    *LIVE_TX.lock().unwrap() = Some(live_tx);
    let ui_ctx = download::Ctx::new();
    let (ui_done_tx, ui_done_rx) = oneshot::channel();
    let rl = renderer.clone();
    let uctx = ui_ctx.clone();
    tokio::spawn(async move {
        rl.run_loop(
            &uctx,
            Duration::from_millis(100),
            live_rx,
            queued_rx,
            ui_done_tx,
        )
        .await;
    });

    install_cancel_handler();

    let cfg = match CString::new(build_config_json(o)) {
        Ok(c) => c,
        Err(_) => {
            eprintln!("odm: go engine: NUL in config JSON");
            return 1;
        }
    };
    let mut buf = vec![0u8; RESULT_CAP];
    unsafe {
        odm_go_set_progress_callback(on_progress);
        odm_go_run(
            cfg.as_ptr(),
            buf.as_mut_ptr() as *mut c_char,
            RESULT_CAP as c_int,
        );
    }

    ui_ctx.cancel();
    let _ = ui_done_rx.await; // wait for the single final-frame writer
    renderer.frame(None, None);
    drop(renderer);

    let used = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    let raw = String::from_utf8_lossy(&buf[..used]);
    let parsed = serde_json::from_str::<Value>(&raw);
    let cancelled = CANCELLED.load(Ordering::SeqCst);
    let (succeeded, failed, total) = match &parsed {
        Ok(v) => match (
            v.get("succeeded").and_then(Value::as_u64),
            v.get("failed").and_then(Value::as_u64),
        ) {
            (Some(ok), Some(f)) => {
                let (s, f) = (ok as usize, f as usize);
                (s, f, s + f)
            }
            _ if cancelled => (0, 0, o.urls.len()),
            _ => {
                eprintln!("odm: go engine: bad result JSON: missing succeeded/failed");
                return 1;
            }
        },
        Err(_) if cancelled => (0, 0, o.urls.len()),
        Err(e) => {
            eprintln!("odm: go engine: bad result JSON: {e}");
            return 1;
        }
    };

    crate::print_summary(succeeded, failed, total, cancelled);
    if let Ok(mut g) = SESSION.lock() {
        if let Some(f) = g.as_mut() {
            let _ = writeln!(
                f,
                "{}",
                json!({"event":"summary","succeeded":succeeded,"failed":failed,"total":total})
            );
        }
        *g = None;
    }

    if cancelled {
        return download::exit_code::EXIT_CANCELLED;
    }
    if failed == 0 && succeeded > 0 {
        0
    } else if succeeded == 0 && failed > 0 {
        2
    } else if failed > 0 {
        3
    } else {
        1 // no tasks: matches production's general-error code
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use config::default_ptr;

    #[test]
    fn config_json_uses_fixed_keys_and_values() {
        let mut o = default_ptr();
        o.urls = vec!["https://a/x".into()];
        o.max_connection = 64;
        o.task_limit_rate = "2M".into();
        o.load_cookie = "cookies.txt".into();

        let v: Value = serde_json::from_str(&build_config_json(&o)).unwrap();
        let expected = [
            "urls",
            "out_file",
            "dir",
            "connections",
            "max_connections",
            "split_file",
            "chunk_size",
            "retry",
            "retry_wait",
            "timeout",
            "max_redirect",
            "user_agent",
            "headers",
            "load_cookies",
            "referer",
            "proxy",
            "check_certificate",
            "checksum",
            "checksum_url",
            "mirrors",
            "limit_rate",
            "limit_rate_per_task",
            "auto_rename",
            "skip_existing",
            "continue_",
            "profile",
            "split",
            "min_split_size",
            "max_connection_per_server",
            "quiet",
        ];
        assert_eq!(v.as_object().unwrap().len(), expected.len());
        for k in expected {
            assert!(v.get(k).is_some(), "missing config key {k}");
        }
        assert!(
            v.get("continue").is_none(),
            "key must be continue_, not continue"
        );
        assert!(v.get("max-connections").is_none());
        assert!(v.get("task_limit_rate").is_none());

        assert_eq!(v["urls"], json!(["https://a/x"]));
        assert_eq!(v["connections"], json!(5));
        assert_eq!(v["max_connections"], json!(64));
        assert_eq!(v["chunk_size"], json!("4M"));
        assert_eq!(v["limit_rate_per_task"], json!("2M"));
        assert_eq!(v["load_cookies"], json!("cookies.txt"));
        assert_eq!(v["profile"], json!("odm"));
        assert_eq!(v["check_certificate"], json!(true));
        assert_eq!(v["continue_"], json!(true));
        assert_eq!(v["quiet"], json!(false));
    }

    #[test]
    fn progress_event_maps_to_renderer_view() {
        let v: Value = serde_json::from_str(
            r#"{"event":"progress","id":"odm-001","url":"https://a/x","file":"x","state":"active","bytes":10,"total":110,"speed":10.5,"conns":4,"errors":1,"retries":2}"#,
        )
        .unwrap();
        let p = progress_view(&v).unwrap();
        assert_eq!(p.id, "odm-001");
        assert_eq!(p.url, "https://a/x");
        assert_eq!(p.final_url, p.url);
        assert_eq!(p.filename, "x");
        assert_eq!(p.state, download::TaskState::Active);
        assert_eq!(p.total_size, 110);
        assert_eq!(p.bytes_done, 10);
        assert_eq!(p.speed, 10); // Go emits a float; fractional part truncates
        assert_eq!(p.connections, 4);
        assert_eq!(p.errors, 1);
        assert_eq!(p.retries, 2);
        assert_eq!(p.eta, Duration::from_secs(10));
        assert!(!p.single_stream);

        for (state, want) in [
            ("downloading", download::TaskState::Active),
            ("completed", download::TaskState::Completed),
            ("error", download::TaskState::Error),
            ("paused", download::TaskState::Paused),
            ("queued", download::TaskState::Queued),
            ("retrying", download::TaskState::Retrying),
        ] {
            let v = json!({"id":"t","state":state,"bytes":10,"total":10,"speed":0});
            assert_eq!(progress_view(&v).unwrap().state, want, "state {state}");
            assert_eq!(progress_view(&v).unwrap().eta, Duration::ZERO);
        }
    }
}
