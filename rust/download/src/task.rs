//! Rust port of ODM's `internal/download/task.go`: Task lifecycle,
//! ProgressView, TaskState, rateMeasure, Ctx, Error, TaskOptions.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use ratelimit::Limiter;
use storage::File as StorageFile;
use tokio::sync::Notify;
use transport::{Client, ProbeResult};

use crate::chunkqueue::{ChunkQueue, WorkQueue};
use crate::engine::Engine;

pub use crate::task_filename::{derive_filename, flatten_filename, format_seg_size, size_or_unknown};

/// `persistCheckpointInterval` — control-file flush every N chunks.
pub const PERSIST_CHECKPOINT_INTERVAL: i64 = 16;
/// `persistMinInterval` — wall-clock floor for checkpoints.
pub const PERSIST_MIN_INTERVAL: Duration = Duration::from_secs(2);
/// `resumeVerifyChunks` bounds how many completed chunks are sampled.
pub const RESUME_VERIFY_CHUNKS: usize = 4;
/// `resumeProbeLen` — how many bytes of each sampled chunk are compared.
pub const RESUME_PROBE_LEN: i64 = 1024;
/// `stallDecayWindow` — zero-byte window before displayed speed decays to 0.
pub const STALL_DECAY_WINDOW: Duration = Duration::from_secs(2);

/// `TaskState` enumerates the lifecycle states (mirrors UI color states).
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum TaskState {
    Queued,
    Active,
    Paused,
    Retrying,
    Completed,
    Error,
}

impl TaskState {
    pub fn as_str(&self) -> &'static str {
        match self {
            TaskState::Queued => "queued",
            TaskState::Active => "downloading",
            TaskState::Paused => "paused",
            TaskState::Retrying => "retrying",
            TaskState::Completed => "completed",
            TaskState::Error => "error",
        }
    }
}

impl std::fmt::Display for TaskState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// `TaskID` is the GID-style per-task id (aria2-compatible-ish).
pub type TaskID = String;

/// `Sink` receives periodic task snapshots (UI/RPC aggregation).
pub type Sink = Arc<dyn Fn(ProgressView) + Send + Sync>;

/// `LogFn` is the leveled-log callback used by Task: level (info/warn/error)
/// + printf-style message.
pub type LogFn = Arc<dyn Fn(&str, String) + Send + Sync>;

/// `ProgressView` is a snapshot of a task's live state.
#[derive(Debug, Clone)]
pub struct ProgressView {
    pub id: TaskID,
    pub url: String,
    pub final_url: String,
    pub filename: String,
    pub state: TaskState,
    pub total_size: i64,
    pub bytes_done: i64,
    pub speed: i64, // bytes/sec, averaged
    pub connections: i32,
    pub eta: Duration,
    pub errors: i32,
    pub retries: i32,
    pub single_stream: bool,
}

/// `TaskOptions` are the parts of *config::Options a Task needs.
#[derive(Debug, Clone, Default)]
pub struct TaskOptions {
    pub output_name: String, // "" ⇒ derive from URL
    pub dir: String,
    pub retry: i32,
    pub retry_wait: Duration,
    pub continue_: bool,
    pub chunk_size: i64,
    pub timeout: Duration,
    pub user_agent: String,
    pub checksum: String,
    pub task_limit_rate: String,

    pub profile: String,
    pub split: i32,
    pub min_split_size: i64,
    pub max_conn_per_server: i32,

    pub collision: String, // ""|"rename"|"skip"
    pub checksum_url: String,
    pub mirrors: Vec<String>,
}

/// `rateMeasure` — rolling window of bytes vs time for a stable speed.
#[derive(Debug, Default)]
pub struct RateMeasure {
    pub(crate) last_ts: Option<Instant>,
    pub(crate) last_bytes: i64,
    pub(crate) bps: i64,
}

impl RateMeasure {
    /// Advances the measure by delta bytes; returns true on ~100ms gates.
    pub fn tick(&mut self, delta: i64) -> bool {
        self.last_bytes += delta;
        let t = Instant::now();
        if self.last_ts.is_none() {
            self.last_ts = Some(t);
            return false;
        }
        let elapsed = t - self.last_ts.unwrap();
        if elapsed < Duration::from_millis(100) {
            return false;
        }
        if elapsed.as_secs_f64() > 0.0 {
            let bps = (self.last_bytes as f64 / elapsed.as_secs_f64()) as i64;
            if bps > 0 {
                if self.bps == 0 {
                    self.bps = bps;
                } else {
                    self.bps = (self.bps as f64 * 0.3 + bps as f64 * 0.7) as i64;
                }
            } else if elapsed >= STALL_DECAY_WINDOW {
                self.bps = 0;
            }
        }
        self.last_ts = Some(t);
        self.last_bytes = 0;
        true
    }
}

/// `Ctx` is the Rust analog of context.Context: cancellation + broadcast wait.
/// `with_parent` mirrors context.WithCancel — child cancels when the parent
/// cancels; `cancel` on the child cancels just the child.
#[derive(Clone, Default)]
pub struct Ctx {
    inner: Arc<CtxInner>,
}

#[derive(Default)]
struct CtxInner {
    cancelled: AtomicBool,
    notify: Notify,
    parent: Mutex<Option<Ctx>>,
}

impl Ctx {
    pub fn new() -> Ctx {
        Ctx::default()
    }
    pub fn with_parent(parent: &Ctx) -> Ctx {
        let c = Ctx::default();
        *c.inner.parent.lock().unwrap() = Some(parent.clone());
        c
    }
    pub fn is_cancelled(&self) -> bool {
        if self.inner.cancelled.load(Ordering::SeqCst) {
            return true;
        }
        self.inner
            .parent
            .lock()
            .unwrap()
            .as_ref()
            .map(|p| p.is_cancelled())
            .unwrap_or(false)
    }
    pub fn cancel(&self) {
        self.inner.cancelled.store(true, Ordering::SeqCst);
        self.inner.notify.notify_waiters();
    }
    /// `wait` blocks until cancelled — own cancel, or a parent's cancel
    /// (context.WithCancel propagation).
    pub async fn wait(&self) {
        loop {
            if self.is_cancelled() {
                return;
            }
            let n = self.inner.notify.notified();
            if self.is_cancelled() {
                return;
            }
            let parent = self.inner.parent.lock().unwrap().clone();
            match parent {
                Some(p) => {
                    let pf = Box::pin(p.wait());
                    tokio::pin!(pf);
                    tokio::select! {
                        _ = pf => {},
                        _ = n => {},
                    }
                }
                None => n.await,
            }
        }
    }
}

/// `Error` is the download error type (mirrors Go's use of errors + StatusError).
#[derive(Debug, Clone)]
pub struct Error {
    pub msg: String,
    pub status: Option<u16>,
    pub permanent: bool,
}

impl Error {
    pub fn msg(m: impl Into<String>) -> Error {
        Error { msg: m.into(), status: None, permanent: false }
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.msg)
    }
}

impl From<transport::StatusError> for Error {
    fn from(e: transport::StatusError) -> Error {
        Error { msg: e.err, status: Some(e.status), permanent: e.permanent }
    }
}

impl From<String> for Error {
    fn from(m: String) -> Error {
        Error::msg(m)
    }
}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Error {
        Error::msg(e.to_string())
    }
}

impl std::error::Error for Error {}

impl TryFrom<i32> for TaskState {
    type Error = ();
    fn try_from(v: i32) -> Result<TaskState, ()> {
        match v {
            0 => Ok(TaskState::Queued),
            1 => Ok(TaskState::Active),
            2 => Ok(TaskState::Paused),
            3 => Ok(TaskState::Retrying),
            4 => Ok(TaskState::Completed),
            5 => Ok(TaskState::Error),
            _ => Err(()),
        }
    }
}

pub fn is_permanent(e: &Error) -> bool {
    e.permanent
}

pub fn status_err(msg: impl Into<String>, status: u16) -> Error {
    Error { msg: msg.into(), status: Some(status), permanent: is_permanent_status(status) }
}

fn is_permanent_status(status: u16) -> bool {
    (400..500).contains(&status) && status != 408 && status != 429
}

/// `Task` represents one file's download (a file with a chunk queue + engine).
pub struct Task {
    pub(crate) id: TaskID,
    pub(crate) url: String,
    pub(crate) opts: Mutex<TaskOptions>,
    pub(crate) client: Arc<Client>,
    pub(crate) lim: Arc<Limiter>,
    pub(crate) task_lim: Mutex<Option<Arc<Limiter>>>,

    /// Sent as If-Range on every ranged GET after a validated resume.
    pub(crate) resume_etag: Mutex<String>,
    pub(crate) probe: Mutex<Option<ProbeResult>>,
    // Arc so workers can capture the file once and call positioned writes
    // without a global lock (Go's os.File.WriteAt is lock-free).
    pub(crate) disk: Mutex<Option<Arc<StorageFile>>>,
    pub(crate) queue: Mutex<Option<Arc<dyn WorkQueue>>>,
    pub(crate) out_path: Mutex<String>,

    /// aria2c profile segment count (0 for odm).
    pub(crate) aria_split: AtomicI64,

    // both profile: engines[0] = odm region1 (h1), engines[1] = aria2c
    // region2 (h2). nil for other profiles.
    pub(crate) engines: Mutex<Vec<Engine>>,
    pub(crate) region_conns: Mutex<Vec<i32>>,
    pub(crate) split_at: AtomicI64,

    pub(crate) single: Mutex<Option<Engine>>,
    pub(crate) h2_client: Mutex<Option<Arc<Client>>>,

    pub(crate) bytes_done: AtomicI64,
    pub(crate) speed: AtomicI64,
    pub(crate) conns: AtomicI32,
    pub(crate) state: AtomicI32,
    pub(crate) errors: AtomicI32,
    pub(crate) retries: AtomicI32,

    pub(crate) chunks_since_persist: Mutex<i64>,
    pub(crate) last_persist: Mutex<Option<Instant>>,
    pub(crate) control_created_at: Mutex<Option<String>>, // RFC3339, preserved across checkpoints

    pub(crate) rm: Mutex<RateMeasure>,

    pub(crate) cancel_ctx: Mutex<Option<Ctx>>,
    pub(crate) cancelled: AtomicBool,
    pub(crate) pause_c: Mutex<Option<Arc<Notify>>>,
    pub(crate) paused: Mutex<bool>,
    pub(crate) logf: LogFn,

    pub(crate) mu: Mutex<()>,
    pub(crate) chunk_hashes: Mutex<HashMap<i64, String>>,

    pub(crate) conn_target: AtomicI32,
    pub(crate) base_ctx: Mutex<Option<Ctx>>,
    pub(crate) sink: Mutex<Option<Sink>>,
    pub(crate) worker_wg: Mutex<Vec<tokio::task::JoinHandle<()>>>,

    pub(crate) mirror_idx: AtomicU64,
    pub(crate) persist_warned: AtomicBool,

    pub(crate) adjust_done: Mutex<bool>,
}

impl Task {
    /// `new_task` — Go `NewTask`. (Clamps Retry/RetryWait < 0; the global
    /// limiter's task limiter comes from ratelimit::new(opts.task_limit_rate).)
    pub fn new(
        id: TaskID,
        url: &str,
        opts: TaskOptions,
        client: Arc<Client>,
        lim: Arc<Limiter>,
        logf: Option<LogFn>,
    ) -> Arc<Task> {
        let logf = logf.unwrap_or_else(|| Arc::new(|_, _| {}));
        let mut opts = opts;
        if opts.retry < 0 {
            opts.retry = 0;
        }
        if opts.retry_wait < Duration::ZERO {
            opts.retry_wait = Duration::ZERO;
        }
        let task_lim = ratelimit::new(&opts.task_limit_rate).ok().map(Arc::new);
        Arc::new(Task {
            id,
            url: url.to_string(),
            opts: Mutex::new(opts),
            client,
            lim,
            task_lim: Mutex::new(task_lim),
            resume_etag: Mutex::new(String::new()),
            probe: Mutex::new(None),
            disk: Mutex::new(None),
            queue: Mutex::new(None),
            out_path: Mutex::new(String::new()),
            aria_split: AtomicI64::new(0),
            engines: Mutex::new(Vec::new()),
            region_conns: Mutex::new(Vec::new()),
            split_at: AtomicI64::new(0),
            single: Mutex::new(None),
            h2_client: Mutex::new(None),
            bytes_done: AtomicI64::new(0),
            speed: AtomicI64::new(0),
            conns: AtomicI32::new(0),
            state: AtomicI32::new(TaskState::Queued as i32),
            errors: AtomicI32::new(0),
            retries: AtomicI32::new(0),
            chunks_since_persist: Mutex::new(0),
            last_persist: Mutex::new(None),
            control_created_at: Mutex::new(None),
            rm: Mutex::new(RateMeasure::default()),
            cancel_ctx: Mutex::new(None),
            cancelled: AtomicBool::new(false),
            pause_c: Mutex::new(None),
            paused: Mutex::new(false),
            logf,
            mu: Mutex::new(()),
            chunk_hashes: Mutex::new(HashMap::new()),
            conn_target: AtomicI32::new(0),
            base_ctx: Mutex::new(None),
            sink: Mutex::new(None),
            worker_wg: Mutex::new(Vec::new()),
            mirror_idx: AtomicU64::new(0),
            persist_warned: AtomicBool::new(false),
            adjust_done: Mutex::new(false),
        })
    }

    pub fn id(&self) -> TaskID {
        self.id.clone()
    }

    /// `snapshot` — read API (UI/RPC).
    pub fn snapshot(&self) -> ProgressView {
        let probe = self.probe.lock().unwrap().clone();
        ProgressView {
            id: self.id.clone(),
            url: self.url.clone(),
            filename: self.filename(),
            state: TaskState::try_from(self.state.load(Ordering::SeqCst)).unwrap_or(TaskState::Queued),

            bytes_done: self.bytes_done.load(Ordering::SeqCst),
            speed: self.speed.load(Ordering::SeqCst),
            connections: self.conns.load(Ordering::SeqCst),
            errors: self.errors.load(Ordering::SeqCst),
            retries: self.retries.load(Ordering::SeqCst),
            total_size: probe.as_ref().map(|p| p.total_size).unwrap_or(0),
            final_url: probe.as_ref().map(|p| p.final_url.clone()).unwrap_or_default(),
            single_stream: probe.as_ref().map(|p| p.single_stream).unwrap_or(false),
            eta: self.estimate_eta(),
        }
    }

    fn filename(&self) -> String {
        let probe = self.probe.lock().unwrap();
        if let Some(pr) = &*probe {
            return pr.filename.clone();
        }
        self.options().output_name.clone()
    }

    /// `output_path` — the full destination path.
    pub fn output_path(&self) -> String {
        self.out_path.lock().unwrap().clone()
    }

    /// `state` — current lifecycle state.
    pub fn state(&self) -> TaskState {
        TaskState::try_from(self.state.load(Ordering::SeqCst)).unwrap_or(TaskState::Queued)
    }

    pub(crate) fn set_state(&self, s: TaskState) {
        self.state.store(s as i32, Ordering::SeqCst);
    }

    /// `emit_final` — one last snapshot carrying the terminal state through
    /// the progressSink before Start returns.
    pub fn emit_final(&self, sink: Option<Sink>) {
        if let Some(s) = sink {
            s(self.snapshot());
        }
    }

    /// `set_conns` — Balancer per-file allocation.
    pub fn set_conns(&self, n: i32) {
        self.conns.store(n, Ordering::SeqCst);
        self.conn_target.store(n, Ordering::SeqCst);
    }

    /// `adjust_conns` — runtime connection change (RPC changeOption "connections").
    ///
    /// Port of Go `Task.AdjustConns`: target<1 clamps to 1 (the RPC boundary
    /// rejects <1 anyway; Start reports a half-empty file as completed, so one
    /// live worker is always safe). Reduction is graceful — workers self-
    /// terminate via the drain checks at the top of their loop. Increase
    /// spawns new workers guarded by `adjust_done` so it can't race a finished
    /// `start()` — spawns are detached in Rust (no workerWg.Add), so there is
    /// no Add-after-Wait hazard here.
    pub fn adjust_conns(self: &Arc<Self>, target: i32, ctx: Option<Ctx>, sink: Option<Sink>) -> bool {
        let target = target.max(1);
        let ctx = match ctx {
            Some(c) => c,
            None => match self.base_ctx.lock().unwrap().clone() {
                Some(c) => c,
                None => return false,
            },
        };
        let _ = sink; // worker reads sink_snapshot() internally; kept for API parity
        let old = self.conn_target.swap(target, Ordering::SeqCst);
        if target <= old {
            return true;
        }
        if *self.adjust_done.lock().unwrap() {
            return false;
        }
        for _ in old..target {
            let me = self.clone();
            let ctx2 = ctx.clone();
            tokio::spawn(async move {
                let _ = me.worker(ctx2, me.current_engine()).await;
            });
        }
        true
    }

    /// `set_h2_client` — attach the HTTP/2 client for the both profile's
    /// region2. Must be called before start.
    pub fn set_h2_client(&self, c: Option<Arc<Client>>) {
        if let Some(c) = c {
            *self.h2_client.lock().unwrap() = Some(c);
        }
    }

    /// `set_probe` — attach a pre-computed probe (CLI one-shot path).
    pub fn set_probe(&self, pr: Option<ProbeResult>) {
        if let Some(pr) = pr {
            *self.probe.lock().unwrap() = Some(pr);
        }
    }

    /// `set_profile` — pin a concrete engine profile (CLI path, smart).
    pub fn set_profile(&self, p: &str) {
        if !p.is_empty() {
            self.opts.lock().unwrap().profile = p.to_string();
        }
    }

    /// `sink_snapshot` — snapshot of the sink (nil-safe).
    pub(crate) fn sink_snapshot(&self) -> Option<Sink> {
        self.sink.lock().unwrap().clone()
    }

    /// `set_probe` (mut version) — n/a.
    /// `options` — locked view of opts.
    pub(crate) fn options(&self) -> std::sync::MutexGuard<'_, TaskOptions> {
        self.opts.lock().unwrap()
    }

    /// `profile_uses_h2` — whether the profile speaks HTTP/2.
    pub(crate) fn profile_uses_h2(&self) -> bool {
        // Clone once: overlapping MutexGuards in one expression deadlock.
        let p = self.options().profile.clone();
        p == "aria2c" || p == "both"
    }

    /// `engine_client` — picks the transport client by engine kind.
    pub(crate) fn engine_client(&self, static_: bool) -> Arc<Client> {
        if static_ {
            if let Some(c) = self.h2_client.lock().unwrap().as_ref() {
                return c.clone();
            }
        }
        self.client.clone()
    }

    /// `current_engine` — the engine a single-profile task's workers use.
    pub fn current_engine(self: &Arc<Self>) -> Engine {
        let engines = self.engines.lock().unwrap();
        if let Some(e) = engines.first() {
            return e.clone();
        }
        let mut single = self.single.lock().unwrap();
        if single.is_none() {
            // Go builds the engine from `t.queue` (the layout start built).
            // An empty placeholder here makes every worker see an empty queue
            // and report a completed all-zero file.
            let q = self
                .queue
                .lock()
                .unwrap()
                .clone()
                .unwrap_or_else(|| Arc::new(ChunkQueue::new(0, 0)));
            let static_ = q.is_static();
            *single = Some(Engine::new(q, self.engine_client(static_), 0));
        }
        return single.clone().unwrap();
    }

    /// `estimate_eta` — bytes-left / speed (nanoseconds).
    pub fn estimate_eta(&self) -> Duration {
        let (total, done, sp) = {
            let probe = self.probe.lock().unwrap();
            let total = probe.as_ref().map(|p| p.total_size).unwrap_or(0);
            (total, self.bytes_done.load(Ordering::SeqCst), self.speed.load(Ordering::SeqCst))
        };
        if total <= 0 || done >= total || sp <= 0 {
            return Duration::ZERO;
        }
        let eta = Duration::from_nanos(((total - done) as f64 / sp as f64) as u64);
        if eta < Duration::ZERO {
            return Duration::ZERO;
        }
        eta
    }

    /// `total_or_done` — total size, or bytes done if total unknown.
    pub(crate) fn total_or_done(&self) -> i64 {
        let probe = self.probe.lock().unwrap();
        if let Some(p) = &*probe {
            if p.total_size > 0 {
                return p.total_size;
            }
        }
        self.bytes_done.load(Ordering::SeqCst)
    }

    pub(crate) fn logf(&self, level: &str, msg: impl Into<String>) {
        (self.logf)(level, msg.into());
    }
}

// Go ints.
pub type TaskIDAlias = TaskID;

// (derive_filename / flatten_filename re-exported above)

#[cfg(test)]
mod tests {
    use super::*;
    use crate::chunkqueue::ChunkQueue;

    #[test]
    fn current_engine_uses_the_task_queue_layout() {
        // Regression: a placeholder empty queue here made every worker see
        // "queue empty" and report a completed all-zero file.
        let client = Arc::new(transport::new_client(transport::ClientConfig::default()).unwrap());
        let t = Task::new(
            "odm-1".to_string(),
            "http://example.invalid/f.bin",
            TaskOptions::default(),
            client,
            Arc::new(ratelimit::Limiter::default()),
            None,
        );
        *t.queue.lock().unwrap() = Some(Arc::new(ChunkQueue::new(1024, 512)));
        let eng = t.current_engine();
        let c = eng.next().expect("engine must expose the chunks from Task::queue");
        assert_eq!(c.start, 0);
        assert_eq!(c.end, 511);
    }
}
