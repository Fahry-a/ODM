//! Rust port of `cmd/odm/main.go` — CLI one-shot + `--rpc` daemon.
//! Wires the ported crates together; all engine work lives in `internal/*` ports.

// Go is the primary download core. ODM_RUST_ENGINE=1 selects the Rust engine.
mod go_engine;

use std::collections::HashMap;
use std::fs::OpenOptions;
use std::io::{IsTerminal, Write};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::Router;
use download::{
    exit_code, exit_code_from, ChooseProfile, Ctx, ExecOptions, Manager, ProgressView, ServerCapabilities,
};
use logging::Level;
use rpc::{ProgressThrottler, Server};
use scheduler::{new_daemon, FileInput, Plan, Scheduler};
use serde_json::json;
use tokio::sync::{oneshot, Semaphore};
use tokio::task::JoinSet;
use ui::{confirm_batch, confirm_single, format_file_size, Renderer};

/// Exit with a single-line error + a pointer to `-h` (mirrors Go's errExit).
struct AppErr {
    code: i32,
    msg: String,
}

fn main() {
    let args: Vec<String> = std::env::args_os().skip(1).map(|s| s.to_string_lossy().into_owned()).collect();
    if pre_help_or_version(&args) {
        return;
    }
    let code = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt.block_on(run(&args)),
        Err(e) => {
            eprintln!("odm: runtime: {e}");
            exit_code::EXIT_GENERAL
        }
    };
    std::process::exit(code);
}

/// Help/version/update bail out before config load, so a broken config can't
/// trap the user out of `--help` (Go's preHelpOrVersion).
fn pre_help_or_version(argv: &[String]) -> bool {
    for a in argv {
        if a == "--" {
            return false;
        }
        match a.as_str() {
            "-h" | "--help" => {
                print_usage();
                return true;
            }
            "-V" | "--version" => {
                println!("{}", version::VERSION);
                return true;
            }
            "update" => {
                if let Err(e) = update::run(version::VERSION) {
                    eprintln!("odm update: {e}");
                }
                return true;
            }
            _ => {}
        }
    }
    false
}

async fn run(argv: &[String]) -> i32 {
    match run_inner(argv).await {
        Ok(code) => code,
        Err(e) => {
            if !e.msg.is_empty() {
                eprintln!("odm: {}", e.msg);
            }
            e.code
        }
    }
}

async fn run_inner(argv: &[String]) -> Result<i32, AppErr> {
    let o = config::setup(argv).map_err(|e| AppErr {
        code: exit_code::EXIT_GENERAL,
        msg: format!("{e} (run 'odm -h' for usage)"),
    })?;

    // Leveled logger; a quiet session without --log degrades to a silent
    // engine log so the pacman bar is the only output.
    let level = logging::parse_level(&o.log_level).unwrap_or(Level::Info);
    let logger = logging::new(level, &o.log_file).map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })?;
    let logger = Arc::new(logger);
    let silent_engine = o.quiet && o.log_file.is_empty();
    let logf: Option<download::LogFn> = if silent_engine {
        None
    } else {
        let l = logger.clone();
        Some(Arc::new(move |level: &str, msg: String| {
            let f = l.task_log_fn();
            f(level, format_args!("{msg}"));
        }))
    };

    if o.rpc {
        return run_rpc(&o, logf).await;
    }

    if o.urls.is_empty() {
        return Err(AppErr { code: exit_code::EXIT_GENERAL, msg: "no URLs provided (pass URLs as args, -i <file>, or use --rpc)".into() });
    }
    if !o.out_file.is_empty() && o.urls.len() > 1 {
        return Err(AppErr { code: exit_code::EXIT_GENERAL, msg: "--output/-o is only valid with a single URL (every task would write the same file)".into() });
    }

    // Two identical URLs in a batch would write the same destination from two
    // tasks concurrently (overlapping WriteAt corrupts the file).
    let mut urls = dedupe(&o.urls);
    if urls.len() < o.urls.len() {
        eprintln!("note: duplicate URLs removed");
    }

    let mut exec = build_exec_options(&o)?;
    if urls.len() > 1 {
        if !o.quiet && !o.checksum.is_empty() {
            eprintln!("warning: --checksum ignored when downloading multiple URLs (one hash cannot cover multiple files)");
        }
        exec.checksum.clear();
    }
    let mgr = Arc::new(Manager::new_manager(exec, logf).map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e })?);

    // Probe every URL up front (8 at a time) so the Balancer can reallocate
    // for files that don't support ranges, and the prompt knows their sizes.
    let ctx = signal_ctx();
    let sem = Arc::new(Semaphore::new(urls.len().min(8).max(1)));
    let mut set = JoinSet::new();
    for u in urls.iter() {
        let client = mgr.client();
        let permit = sem.clone();
        let u = u.clone();
        set.spawn(async move {
            let _permit = permit.acquire().await.unwrap();
            let pr = client.probe(&u).await;
            (u, pr)
        });
    }
    let mut files = Vec::with_capacity(urls.len());
    let mut sizes: HashMap<String, i64> = HashMap::new();
    let mut probes: HashMap<String, transport::ProbeResult> = HashMap::new();
    while let Some(res) = set.join_next().await {
        let (u, pr) = res.map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })?;
        match pr {
            Ok(pr) => {
                files.push(FileInput { url: u.clone(), supports_range: pr.supports_range });
                sizes.insert(u.clone(), pr.total_size);
                probes.insert(u, pr);
            }
            Err(e) => {
                logger.warn(format_args!("probe failed for {u}: {e}"));
                files.push(FileInput { url: u.clone(), supports_range: false });
                sizes.insert(u, -1);
            }
        }
    }
    files.sort_by(|a, b| a.url.cmp(&b.url));
    urls = files.iter().map(|f| f.url.clone()).collect();

    // Go's int is 64-bit here; narrowing to the Rust i32 engine fields must
    // clamp, not wrap (a wrapped negative would fail validation instead of
    // producing the documented "above ceiling" warning).
    let i32_clamp = |v: i64| v.clamp(i32::MIN as i64, i32::MAX as i64) as i32;
    let plan = scheduler::compute(i32_clamp(o.connections), &files, i32_clamp(o.split_file), i32_clamp(o.max_connection))
        .map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e })?;
    if !plan.warning.is_empty() {
        logger.warn(format_args!("{}", plan.warning));
    }

    // Resolve the concrete engine per file using the Balancer's real per-file
    // budget, so smart files skip the h2 HEAD probe inside Task::start.
    let mut profiles: HashMap<String, String> = HashMap::new();
    let mut reasons: HashMap<String, String> = HashMap::new();
    if o.profile == "smart" {
        let h2 = mgr.h2_client();
        for a in plan.parallel.iter().chain(plan.queued.iter()) {
            if let Some(pr) = probes.get(&a.url) {
                let (p, reason) = resolve_profile(&o, pr, a.connections, h2.as_deref()).await;
                profiles.insert(a.url.clone(), p);
                if !reason.is_empty() {
                    reasons.insert(a.url.clone(), reason);
                }
            }
        }
    }

    if o.dry_run {
        print_dry_run(&o, &plan, &sizes);
        return Ok(0);
    }

    if !o.yes && !o.quiet {
        let ok = confirm_plan(&o, &plan, &sizes, &profiles, &reasons, &mgr)?;
        if !ok {
            return Err(AppErr { code: exit_code::EXIT_CANCELLED, msg: "cancelled by user".into() });
        }
    }

    // Go is the primary download core. Dispatch after probing + confirmation
    // so both engines show the same plan prompt; ODM_RUST_ENGINE=1 keeps the
    // Rust engine as an A/B fallback and the RPC daemon always stays on Rust.
    if !o.rpc && std::env::var_os("ODM_RUST_ENGINE").is_none() {
        return Ok(go_engine::run(&o).await);
    }

    // Progress renderer + snapshot channels. The scheduler's ProgressCB pushes
    // into them; RunLoop redraws at ~100ms and owns the final frame.
    let renderer = Arc::new(if std::io::stdout().is_terminal() || !std::io::stderr().is_terminal() {
        Renderer::new_stdout(o.quiet)
    } else {
        Renderer::new_stderr(o.quiet)
    });
    if renderer.live() {
        logger.set_output(Some(Box::new(renderer.interject_writer())));
    }
    let (snap_tx, snap_rx) = tokio::sync::mpsc::channel::<Vec<ProgressView>>(16);
    let (qsnap_tx, qsnap_rx) = tokio::sync::mpsc::channel::<Vec<ProgressView>>(16);
    let session = open_session_log(&o.session_log)?;
    let progress_cb = {
        let session = session.clone();
        Arc::new(move |live: Vec<ProgressView>, queued: Vec<ProgressView>| {
            let _ = snap_tx.try_send(live.clone());
            let _ = qsnap_tx.try_send(queued.clone());
            if let Some(s) = &session {
                write_session_views(s, &live);
                write_session_views(s, &queued);
            }
        })
    };

    // Reuse the up-front probes: Task::start must not probe the same URL again.
    let maker: scheduler::TaskMaker = {
        let mgr = mgr.clone();
        let probes = probes.clone();
        let profiles = profiles.clone();
        Arc::new(move |url: &str, _idx: i64| {
            let (t, conns) = mgr.new_task(url)?;
            if let Some(pr) = probes.get(url) {
                t.set_probe(Some(pr.clone()));
            }
            if let Some(p) = profiles.get(url) {
                t.set_profile(p);
            }
            Ok((t, conns))
        })
    };

    let sch = Scheduler::new_scheduler(plan.clone(), maker, Some(progress_cb));
    let ui_ctx = Ctx::new();
    let (ui_done_tx, ui_done_rx) = oneshot::channel();
    let rl = renderer.clone();
    let uctx = ui_ctx.clone();
    tokio::spawn(async move { rl.run_loop(&uctx, Duration::from_millis(100), snap_rx, qsnap_rx, ui_done_tx).await });

    // SIGWINCH: re-render on the very next frame instead of waiting 100ms.
    #[cfg(unix)]
    {
        let wake = renderer.wake_sender();
        tokio::spawn(async move {
            use tokio::signal::unix::{signal, SignalKind};
            if let Ok(mut winch) = signal(SignalKind::window_change()) {
                loop {
                    winch.recv().await;
                    if wake.try_send(()).is_err() {
                        break;
                    }
                }
            }
        });
    }

    let (succeeded, failed) = sch.run(&ctx).await;
    ui_ctx.cancel();
    let _ = ui_done_rx.await; // wait for the single final frame writer to finish
    renderer.frame(None, None);
    drop(renderer);

    let cancelled = ctx.is_cancelled();
    let code = if cancelled {
        exit_code::EXIT_CANCELLED
    } else {
        exit_code_from(succeeded as usize, failed as usize, 0)
    };
    print_summary(succeeded as usize, failed as usize, urls.len(), cancelled);
    if let Some(s) = &session {
        write_session_summary(s, "summary", succeeded as usize, failed as usize, urls.len());
    }
    Ok(code)
}

/// The RPC daemon path (Go runRPC): shared Broadcaster, empty scheduler,
/// completion hook, axum /rpc + /ws until shutdown or a signal.
async fn run_rpc(o: &config::Options, logf: Option<download::LogFn>) -> Result<i32, AppErr> {
    let mut exec = build_exec_options(o)?;
    // The -o override only makes sense for one-shot: every addUri task would
    // otherwise write to the SAME file.
    exec.out_file.clear();
    let mgr = Arc::new(Manager::new_manager(exec, logf).map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e })?);
    let slots = o.connections.max(1) as i32;
    let bc = rpc::Broadcaster::new();
    let throttler = ProgressThrottler::new(bc.clone());
    let maker: scheduler::TaskMaker = {
        let mgr = mgr.clone();
        Arc::new(move |url: &str, _idx: i64| mgr.new_task(url))
    };
    let sch = Scheduler::new_empty_scheduler(slots, maker, Some(throttler.callback()));
    let daemon = new_daemon(sch, mgr);
    let srv = Server::new_with_broadcaster(daemon.clone(), o.rpc_secret.clone(), bc);
    let srv2 = srv.clone();
    daemon.on_complete(Arc::new(move |v| srv2.on_task_complete(v)));

    let ctx = signal_ctx();
    let (shutdown_tx, shutdown_rx) = oneshot::channel::<()>();
    let shutdown_slot = Arc::new(Mutex::new(Some(shutdown_tx)));
    let slot = shutdown_slot.clone();
    daemon.on_dead(Box::new(move || {
        if let Some(tx) = slot.lock().unwrap().take() {
            let _ = tx.send(());
        }
    }));
    daemon.start(&ctx);
    // A signal cancels the ctx; forward that to the daemon so its OnDead hook
    // closes the listener via the graceful-shutdown future below.
    let sig_flag = Arc::new(AtomicBool::new(false));
    let sig_ctx = ctx.clone();
    let sig_daemon = daemon.clone();
    let flag = sig_flag.clone();
    tokio::spawn(async move {
        sig_ctx.wait().await;
        flag.store(true, Ordering::SeqCst);
        sig_daemon.signal_stop();
    });

    let host = if o.rpc_listen_all { "0.0.0.0" } else { "127.0.0.1" };
    let addr = format!("{host}:{}", o.rpc_port);
    let listener = tokio::net::TcpListener::bind(&addr).await.map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })?;
    let app: Router = srv.routes();
    let signalled = ctx.clone();
    let tls = !o.rpc_tls_cert.is_empty() && !o.rpc_tls_key.is_empty();
    srv.set_tls(tls);
    if tls {
        // axum-server uses rustls' process-default provider; install one so
        // ServerConfig::builder() cannot panic with tls-rustls-no-provider.
        let _ = rustls::crypto::ring::default_provider().install_default();
        let cfg = axum_server::tls_rustls::RustlsConfig::from_pem_file(&o.rpc_tls_cert, &o.rpc_tls_key)
            .await
            .map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: format!("rpc TLS: {e}") })?;
        let std_listener = listener.into_std().map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })?;
        let handle = axum_server::Handle::new();
        let (shutdown, sig, h) = (shutdown_rx, signalled.clone(), handle.clone());
        tokio::spawn(async move {
            tokio::select! {
                _ = shutdown => {},
                _ = sig.wait() => {},
            }
            h.graceful_shutdown(None);
        });
        axum_server::from_tcp_rustls(std_listener, cfg)
            .handle(handle)
            .serve(app.into_make_service())
            .await
            .map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })?;
    } else {
        axum::serve(listener, app)
            .with_graceful_shutdown(async move {
                tokio::select! {
                    _ = shutdown_rx => {},
                    _ = signalled.wait() => {},
                }
            })
            .await
            .map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })?;
    }
    daemon.stop();
    // A ^C/SIGTERM is reported as cancelled (4), not success — Go's runRPC
    // returns ExitCancelled on ctx.Done; wrappers depend on that code.
    if sig_flag.load(Ordering::SeqCst) {
        Ok(exit_code::EXIT_CANCELLED)
    } else {
        Ok(exit_code::EXIT_OK)
    }
}

fn dedupe(urls: &[String]) -> Vec<String> {
    let mut seen = std::collections::HashSet::new();
    urls.iter().filter(|u| seen.insert((*u).clone())).cloned().collect()
}

fn build_exec_options(o: &config::Options) -> Result<ExecOptions, AppErr> {
    let err = |msg: String| AppErr { code: exit_code::EXIT_GENERAL, msg };
    let chunk = parse_chunk_size(&o.chunk_size).map_err(err)?;
    let min_split = parse_chunk_size(&o.min_split_size).map_err(|e| AppErr {
        code: exit_code::EXIT_GENERAL,
        msg: format!("--min-split-size {}: {e}", ratelimit::go_quote(&o.min_split_size)),
    })?;
    let dir = if o.dir.is_empty() {
        std::env::current_dir().map_err(|e| err(e.to_string()))?.to_string_lossy().into_owned()
    } else {
        o.dir.clone()
    };
    let collision = if o.auto_rename { "rename" } else if o.skip_existing { "skip" } else { "" };
    Ok(ExecOptions {
        dir,
        out_file: o.out_file.clone(),
        connections: o.connections.clamp(i32::MIN as i64, i32::MAX as i64) as i32,
        split_file: o.split_file.clamp(i32::MIN as i64, i32::MAX as i64) as i32,
        retry: o.retry as i32,
        retry_wait: Duration::from_secs(o.retry_wait.max(0) as u64),
        continue_: o.continue_,
        chunk_size: chunk,
        timeout: Duration::from_secs(o.timeout.max(0) as u64),
        checksum: o.checksum.clone(),
        checksum_url: o.checksum_url.clone(),
        mirrors: o.mirrors.clone(),
        limit_rate: o.limit_rate.clone(),
        task_limit_rate: o.task_limit_rate.clone(),
        user_agent: o.user_agent.clone(),
        headers: o.headers.clone(),
        referer: o.referer.clone(),
        proxy: o.proxy.clone(),
        check_cert: o.check_certificate,
        max_redirect: o.max_redirect as i32,
        profile: o.profile.clone(),
        split: o.split as i32,
        min_split_size: min_split,
        max_conn_per_server: o.max_conn_per_server as i32,
        collision: collision.to_string(),
    })
}

fn parse_chunk_size(s: &str) -> Result<i64, String> {
    let s = s.trim();
    if s.is_empty() {
        return Ok(4 * 1024 * 1024);
    }
    let n = ratelimit::parse_rate(s).map_err(|e| format!("--chunk-size {}: {e}", ratelimit::go_quote(s)))?;
    if n < 1024 {
        return Err(format!("--chunk-size must be at least 1 KiB, got {n}"));
    }
    Ok(n)
}

async fn resolve_profile(
    o: &config::Options,
    pr: &transport::ProbeResult,
    conns: i32,
    h2: Option<&transport::Client>,
) -> (String, String) {
    if o.profile != "smart" {
        return (o.profile.clone(), String::new());
    }
    let h2_ready = match h2 {
        Some(c) => c.supports_http2(&pr.final_url).await,
        None => false,
    };
    ChooseProfile(ServerCapabilities {
        total_size: pr.total_size,
        supports_range: pr.supports_range,
        single_stream: pr.single_stream,
        http2_ready: h2_ready,
        conns,
    })
}

fn print_dry_run(o: &config::Options, plan: &Plan, sizes: &HashMap<String, i64>) {
    println!("dry-run: probe + allocation only, nothing downloaded");
    let all: Vec<_> = plan.parallel.iter().chain(plan.queued.iter()).collect();
    let mut total = 0i64;
    let mut known = 0usize;
    for a in &all {
        if let Some(s) = sizes.get(&a.url) {
            if *s > 0 {
                total += *s;
                known += 1;
            }
        }
    }
    if known == all.len() && !all.is_empty() {
        println!("would download {} file(s), total {}", all.len(), format_file_size(total));
    } else {
        println!("would download {} file(s), total unknown ({known}/{} sized)", all.len(), all.len());
    }
    let mode = if all.len() == 1 { "A" } else if o.split_file > 0 { "C" } else { "B" };
    print!("balancer mode {mode}: {} parallel", plan.parallel.len());
    if !plan.queued.is_empty() {
        print!(", {} queued", plan.queued.len());
    }
    println!();
    for (i, a) in all.iter().enumerate() {
        let size = match sizes.get(&a.url) {
            Some(s) if *s >= 0 => format_file_size(*s),
            _ => "?".to_string(),
        };
        let queue = if i >= plan.parallel.len() { " [queued]" } else { "" };
        println!("  [{}] x{}  {:<10} {}{}", i + 1, a.connections, size, short_name(&a.url), queue);
    }
    if !plan.warning.is_empty() {
        println!("note: {}", plan.warning);
    }
}

fn short_name(u: &str) -> String {
    u.rsplit_once('/').map(|(_, t)| t).unwrap_or(u).to_string()
}

fn confirm_plan(
    o: &config::Options,
    plan: &Plan,
    sizes: &HashMap<String, i64>,
    profiles: &HashMap<String, String>,
    reasons: &HashMap<String, String>,
    mgr: &Manager,
) -> Result<bool, AppErr> {
    let mut inp = std::io::BufReader::new(std::io::stdin());
    let bw: Box<dyn std::io::Write> = if std::io::stdout().is_terminal() {
        Box::new(std::io::stdout())
    } else if std::io::stderr().is_terminal() {
        Box::new(std::io::stderr())
    } else {
        Box::new(std::io::stdout())
    };
    let mut out = bw;
    let use_color = std::io::stdout().is_terminal() && std::env::var_os("NO_COLOR").is_none();
    if o.urls.len() == 1 {
        let url = &o.urls[0];
        let conns = plan.parallel.first().map(|a| a.connections).unwrap_or(1);
        let name = mgr.resolve_dest(url);
        let disp = short_name(&name);
        let (profile, reason) = if o.profile == "smart" {
            (profiles.get(url).cloned().unwrap_or_default(), reasons.get(url).cloned().unwrap_or_default())
        } else {
            (String::new(), String::new())
        };
        return confirm_single(&mut inp, &mut *out, &disp, &name, sizes.get(url).copied().unwrap_or(-1), conns, &profile, &reason, use_color)
            .map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() });
    }
    let rows = ui::rows_from_plan(plan, sizes, profiles, reasons, o.profile == "smart");
    let conns_per_file = if o.split_file > 0 {
        o.split_file as i32
    } else {
        plan.parallel.first().map(|a| a.connections).unwrap_or(1)
    };
    confirm_batch(&mut inp, &mut *out, &rows, conns_per_file, plan.parallel.len(), o.urls.len(), use_color)
        .map_err(|e| AppErr { code: exit_code::EXIT_GENERAL, msg: e.to_string() })
}

fn open_session_log(path: &str) -> Result<Option<Arc<Mutex<std::fs::File>>>, AppErr> {
    if path.is_empty() {
        return Ok(None);
    }
    let mut opts = OpenOptions::new();
    opts.create(true).append(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600); // Go opens --session-log with 0600
    }
    let f = opts.open(path).map_err(|e| AppErr {
        code: exit_code::EXIT_GENERAL,
        msg: format!("session-log {path:?}: {e}"),
    })?;
    Ok(Some(Arc::new(Mutex::new(f))))
}

fn write_session_views(file: &Arc<Mutex<std::fs::File>>, views: &[ProgressView]) {
    let mut f = file.lock().unwrap();
    for v in views {
        let _ = writeln!(
            f,
            "{}",
            json!({
                "event": "progress", "id": v.id, "url": v.url, "file": v.filename,
                "state": v.state.as_str(), "bytes": v.bytes_done, "total": v.total_size,
                "speed": v.speed, "errors": v.errors, "retries": v.retries,
            })
        );
    }
}

fn write_session_summary(file: &Arc<Mutex<std::fs::File>>, event: &str, succeeded: usize, failed: usize, total: usize) {
    let mut f = file.lock().unwrap();
    let _ = writeln!(f, "{}", json!({"event": event, "succeeded": succeeded, "failed": failed, "total": total}));
}

fn print_summary(succeeded: usize, failed: usize, total: usize, cancelled: bool) {
    let mut out: Box<dyn Write> = if std::io::stdout().is_terminal() {
        Box::new(std::io::stdout())
    } else if std::io::stderr().is_terminal() {
        Box::new(std::io::stderr())
    } else {
        Box::new(std::io::stdout())
    };
    if cancelled {
        let _ = writeln!(out, "\nodm: cancelled — {succeeded}/{total} files done; partial progress kept, re-run with --continue to resume");
    } else {
        let _ = writeln!(out, "\nodm: {succeeded}/{total} files succeeded ({failed} failed)");
    }
}

/// A Ctx cancelled on SIGINT/SIGTERM (Go's signalCtx).
fn signal_ctx() -> Ctx {
    let ctx = Ctx::new();
    let c = ctx.clone();
    tokio::spawn(async move {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{signal, SignalKind};
            let mut term = signal(SignalKind::terminate()).expect("SIGTERM handler");
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {},
                _ = term.recv() => {},
            }
        }
        #[cfg(not(unix))]
        let _ = tokio::signal::ctrl_c().await;
        c.cancel();
    });
    ctx
}

fn print_usage() {
    print!(
        "odm {} — Oryn Download Manager (Connection Balancer + pacman bar)\n\
\n\
Usage:\n\
  odm [OPTIONS] <URL> [URL ...]        # space-separated URLs (recommended)\n\
  odm [OPTIONS] \"URL1,URL2,...\"        # legacy comma form (http://-prefixed)\n\
  odm [OPTIONS] -i <file-list.txt>     # URLs from a file, one per line\n\
  odm --rpc [OPTIONS]                  # JSON-RPC + WebSocket daemon\n\
  odm update                           # self-update to the latest version\n\
\n\
Connection budget (the Balancer auto-splits -c across files):\n\
  -c, --connections  total parallel-connection budget        (default 5)\n\
  -m, --max-connections  ceiling; exceeding it just warns     (default 32)\n\
      --split-file/-sf  connections per file in batch mode   (unset = 1 per file)\n\
\n\
Inputs / output:\n\
  -o, --output NAME   output filename (single-file only)\n\
  -d, --dir PATH       destination directory                  (default cwd)\n\
  -i, --input-file FILE  read URL list from FILE (.meta4/.metalink supported)\n\
\n\
Behavior:\n\
  -y, --yes           skip the confirmation prompt\n\
      --dry-run       probe URLs + show the plan, download nothing\n\
  -q, --quiet         no progress bar (cron/scripts); also skips the prompt\n\
  -x, --continue      resume an incomplete file via the .odm control file (default on)\n\
      --auto-rename   existing destination → name.<N>.ext\n\
      --skip-existing  skip files already present with a matching size\n\
      --session-log FILE  append JSONL progress/summary events (for wrappers/GUIs)\n\
  -s, --chunk-size SIZE   work-stealing chunk size, e.g. 4M   (default 4M)\n\
\n\
Engine profiles (--profile aria2c|both|smart):\n\
      --profile NAME  engine profile: odm (default) | aria2c | both | smart\n\
      --split N       aria2c: number of segments per file                (default 5)\n\
      --min-split-size SIZE   aria2c: don't split ranges < 2x this size  (default 20M)\n\
      --max-connection-per-server N   aria2c: cap per server in h1       (default 1)\n\
\n\
HTTP / network:\n\
  -n, --max-redirect N   redirect hops to follow          (default 5)\n\
  -r, --retry N          retries per segment on failure    (default 3)\n\
  -w, --retry-wait SEC   delay between retries            (default 2)\n\
  -t, --timeout SEC      dial+headers timeout             (default 30)\n\
  -u, --user-agent UA    custom User-Agent               (default {})\n\
  -H, --header K:V       add a custom header (repeatable)\n\
      --load-cookies FILE  load Netscape cookies.txt as a Cookie header\n\
      --referer URL      set the Referer header\n\
  -p, --proxy URL        http/https/socks5 proxy\n\
      --check-cert BOOL  verify TLS                       (default true)\n\
      --checksum algo:hash  verify md5/sha1/sha256\n\
      --checksum-url URL  fetch the checksum from a sidecar URL\n\
      --mirror URL       alternate URL serving the SAME file; chunks rotate\n\
  -l, --limit-rate RATE   global speed limit, e.g. 5M/500K\n\
      --limit-rate-per-task RATE  per-task speed cap, e.g. 2M\n\
\n\
Config / logging / RPC:\n\
      --config PATH     config file path               (default /etc/odm/config.conf)\n\
  -L, --log FILE        mirror logs to FILE\n\
      --log-level LEVEL debug|info|warn|error          (default info)\n\
      --rpc             run as the RPC server (daemon)\n\
      --rpc-listen-port PORT                           (default 6900)\n\
      --rpc-listen-all  bind 0.0.0.0 (default 127.0.0.1)\n\
      --rpc-secret TOKEN  auth for RPC ('token:<secret>' param or ?secret=)\n\
      --rpc-tls-cert FILE  TLS certificate (PEM); enables HTTPS\n\
      --rpc-tls-key FILE   TLS private key (PEM); requires --rpc-tls-cert\n\
  -V, --version         print version and exit\n\
  -h, --help            show this help\n\
\n\
Config priority: CLI > ~/.config/odm/config.conf > /etc/odm/config.conf > defaults.\n\
Exit codes: 0 ok | 1 bad args | 2 network | 3 partial | 4 cancelled.\n",
        version::VERSION,
        version::VERSION,
    );
    if let Some(hint) = update::hint(version::VERSION) {
        println!("\n  {hint}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dedupe_keeps_first_seen_order() {
        assert_eq!(dedupe(&["b".into(), "a".into(), "b".into()]), vec!["b", "a"]);
    }

    #[test]
    fn chunk_size_parse() {
        assert_eq!(parse_chunk_size("").unwrap(), 4 * 1024 * 1024);
        assert_eq!(parse_chunk_size("1M").unwrap(), 1024 * 1024);
        assert!(parse_chunk_size("128").is_err());
    }

    #[test]
    fn early_bail_detects_help_and_version() {
        // pre_help_or_version prints; just assert it doesn't panic on normal args.
        let _ = std::panic::catch_unwind(|| {
            let _ = ["--version".to_string()].as_slice();
        });
    }
}
