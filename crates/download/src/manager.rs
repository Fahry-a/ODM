//! Rust port of ODM's `internal/download/manager.go`: Manager, ExecOptions,
//! task registry (RPC tellActive), verifyChecksum, ExitCodeFrom, ResolveDest.

use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use ratelimit::Limiter;
use transport::Client;

use crate::task::{Error, Task, TaskID};

/// `ExecOptions` is the subset of *config::Options the Manager exercises.
#[derive(Debug, Clone, Default)]
pub struct ExecOptions {
    pub dir: String,
    pub out_file: String, // single-file override (-o)
    pub connections: i32,
    pub split_file: i32,
    pub retry: i32,
    pub retry_wait: Duration,
    pub continue_: bool,
    pub chunk_size: i64,
    pub timeout: Duration,
    pub checksum: String,
    pub checksum_url: String,
    pub mirrors: Vec<String>,
    pub limit_rate: String,
    pub task_limit_rate: String,
    pub user_agent: String,
    pub headers: Vec<String>,
    pub referer: String,
    pub proxy: String,
    pub check_cert: bool,
    pub max_redirect: i32,
    pub profile: String,
    pub split: i32,
    pub min_split_size: i64,
    pub max_conn_per_server: i32,
    pub collision: String,
}

/// `Manager` glues transport + tasks + scheduler together for the CLI path
/// (and is reused by the RPC daemon in reference form). It builds tasks from
/// URLs, tracks them by id, owns the shared transport client + global rate
/// limiter (one pool spans the whole batch), and exposes checksum
/// verification.
pub struct Manager {
    pub(crate) client: Arc<Client>,
    pub(crate) h2client: Option<Arc<Client>>,
    pub(crate) lim: Arc<Limiter>,
    pub(crate) opts: ExecOptions,
    pub(crate) logf: Option<crate::task::LogFn>,

    next_id: AtomicI64,
    mu: Mutex<HashMap<TaskID, Arc<Task>>>, // RPC tellActive/tellWaiting
}

const MAX_TRACKED_TASKS: usize = 1000;

impl Manager {
    /// `new_manager` builds a Manager. The underlying transport + limiter are
    /// constructed here so one pool spans the whole batch.
    pub fn new_manager(opts: ExecOptions, logf: Option<crate::task::LogFn>) -> Result<Manager, String> {
        let cli = transport::new_client(transport::ClientConfig {
            user_agent: opts.user_agent.clone(),
            headers: opts.headers.clone(),
            referer: opts.referer.clone(),
            proxy: opts.proxy.clone(),
            check_certificate: opts.check_cert,
            timeout: opts.timeout,
            max_redirect: opts.max_redirect,
            ..Default::default()
        })?;
        let lim = ratelimit::new(&opts.limit_rate)?;
        // The h2-enabled client is only needed by non-odm profiles.
        let h2client = if !opts.profile.is_empty() && opts.profile != "odm" {
            let mut ccfg = transport::ClientConfig {
                user_agent: opts.user_agent.clone(),
                headers: opts.headers.clone(),
                referer: opts.referer.clone(),
                proxy: opts.proxy.clone(),
                check_certificate: opts.check_cert,
                timeout: opts.timeout,
                max_redirect: opts.max_redirect,
                ..Default::default()
            };
            ccfg.http2 = true;
            Some(Arc::new(transport::new_client(ccfg)?))
        } else {
            None
        };
        Ok(Manager {
            client: Arc::new(cli),
            h2client,
            lim: Arc::new(lim),
            opts,
            logf,
            next_id: AtomicI64::new(0),
            mu: Mutex::new(HashMap::new()),
        })
    }

    /// `client` — the transport client (RPC tests reuse it).
    pub fn client(&self) -> Arc<Client> {
        self.client.clone()
    }

    /// `h2client` — the HTTP/2-enabled transport client (aria2c/both/smart),
    /// or None when the profile never builds one.
    pub fn h2_client(&self) -> Option<Arc<Client>> {
        self.h2client.clone()
    }

    /// `client_for` — the transport client for the given profile.
    pub fn client_for(&self, profile: &str) -> Arc<Client> {
        if matches!(profile, "aria2c" | "both" | "smart") {
            if let Some(c) = &self.h2client {
                return c.clone();
            }
        }
        self.client.clone()
    }

    /// `limiter` — the global limiter.
    pub fn limiter(&self) -> Arc<Limiter> {
        self.lim.clone()
    }

    /// `new_task` — the TaskMaker the Scheduler calls.
    pub fn new_task(&self, url: &str) -> Result<(Arc<Task>, i32), Error> {
        let id = format!("odm-{:03}", self.next_id.fetch_add(1, Ordering::SeqCst) + 1);
        let conns = self.opts.connections;
        let profile = if self.opts.profile.is_empty() { "odm" } else { self.opts.profile.as_str() };
        let t = Task::new(
            id.clone(),
            url,
            crate::task::TaskOptions {
                output_name: self.opts.out_file.clone(),
                dir: self.opts.dir.clone(),
                retry: self.opts.retry,
                retry_wait: self.opts.retry_wait,
                continue_: self.opts.continue_,
                chunk_size: self.opts.chunk_size,
                timeout: self.opts.timeout,
                user_agent: self.opts.user_agent.clone(),
                checksum: self.opts.checksum.clone(),
                checksum_url: self.opts.checksum_url.clone(),
                mirrors: self.opts.mirrors.clone(),
                task_limit_rate: self.opts.task_limit_rate.clone(),
                profile: profile.to_string(),
                split: self.opts.split,
                min_split_size: self.opts.min_split_size,
                max_conn_per_server: self.opts.max_conn_per_server,
                collision: self.opts.collision.clone(),
            },
            self.client.clone(),
            self.lim.clone(),
            self.logf.clone(),
        );
        if let Some(h2) = self.h2client.clone() {
            t.set_h2_client(Some(h2));
        }
        self.track(id, t.clone());
        Ok((t, conns))
    }

    /// `track` — insert into the registry (used by RPC tellActive), pruning
    /// oldest terminal entries once the cap is exceeded.
    fn track(&self, id: TaskID, t: Arc<Task>) {
        let mut guard = self.mu.lock().unwrap();
        guard.insert(id, t);
        if guard.len() > MAX_TRACKED_TASKS {
            let n = guard.len() - MAX_TRACKED_TASKS;
            drop_terminal_tasks(&mut guard, n);
        }
    }

    /// `tasks` — snapshot of all tasks the manager has built.
    pub fn tasks(&self) -> HashMap<TaskID, Arc<Task>> {
        self.mu.lock().unwrap().clone()
    }

    /// `task` — the task with the given id, or None if unknown.
    pub fn task(&self, id: &TaskID) -> Option<Arc<Task>> {
        self.mu.lock().unwrap().get(id).cloned()
    }

    /// `run` — runs a single task directly (Mode A path).
    pub async fn run(&self, ctx: &crate::task::Ctx, url: &str, conns: i32) -> Result<(), Error> {
        let (t, _) = self.new_task(url)?;
        t.start(ctx, conns, None).await
    }

    /// `resolve_dest` — the path a task for `url` would write to (prompt).
    pub fn resolve_dest(&self, url: &str) -> String {
        let mut name = self.opts.out_file.clone();
        if name.is_empty() {
            let u = url.split('?').next().unwrap_or(url);
            name = Path::new(u).file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
            if name.is_empty() || name == "." || name == "/" {
                name = "download.bin".to_string();
            }
        }
        Path::new(&self.opts.dir).join(name).to_string_lossy().into_owned()
    }
}

/// `drop_terminal_tasks` — removes up to n terminal entries, oldest first.
fn drop_terminal_tasks(m: &mut HashMap<TaskID, Arc<Task>>, n: usize) {
    if n == 0 {
        return;
    }
    let mut ids: Vec<(i64, TaskID)> = m
        .iter()
        .filter(|(_, t)| matches!(t.state(), crate::task::TaskState::Completed | crate::task::TaskState::Error))
        .map(|(id, _)| {
            let num = id
                .strip_prefix("odm-")
                .and_then(|s| s.parse::<i64>().ok())
                .unwrap_or(1 << 62);
            (num, id.clone())
        })
        .collect();
    ids.sort_by_key(|(num, _)| *num);
    for (_, id) in ids.into_iter().take(n) {
        m.remove(&id);
    }
}

/// `verify_checksum` — shared checksum core: hash the file at path and
/// compare against expect_hex.
pub fn verify_checksum(path: &str, algo: &str, expect_hex: &str) -> Result<(), Error> {
    use std::io::Read;

    // Stream the file in 64 KiB blocks (Go's io.Copy) instead of loading the
    // whole payload into RAM.
    fn feed_file(path: &str, mut consume: impl FnMut(&[u8])) -> Result<(), Error> {
        let mut file = std::fs::File::open(path).map_err(Error::from)?;
        let mut buf = [0u8; 64 * 1024];
        loop {
            let n = file.read(&mut buf).map_err(Error::from)?;
            if n == 0 {
                break;
            }
            consume(&buf[..n]);
        }
        Ok(())
    }
    fn hex_of(h: &[u8]) -> String {
        h.iter().map(|b| format!("{b:02x}")).collect()
    }
    let got = match algo.to_lowercase().as_str() {
        "md5" => {
            let mut h = md5::Context::new();
            feed_file(path, |b| h.consume(b))?;
            hex_of(&h.compute().0)
        }
        "sha1" => {
            let mut h = <sha1::Sha1 as sha1::digest::Digest>::new();
            feed_file(path, |b| sha1::digest::Digest::update(&mut h, b))?;
            hex_of(&sha1::digest::Digest::finalize(h))
        }
        "sha256" => {
            let mut h = <sha2::Sha256 as sha2::Digest>::new();
            feed_file(path, |b| sha2::Digest::update(&mut h, b))?;
            hex_of(&sha2::Digest::finalize(h))
        }
        _ => return Err(Error::msg(format!("unsupported checksum algo {algo:?}"))),
    };
    // Go uses strings.EqualFold(got, strings.TrimSpace(expectHex)).
    if !got.eq_ignore_ascii_case(expect_hex.trim()) {
        return Err(Error::msg(format!("checksum mismatch: got {got} want {expect_hex}")));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verify_checksum_streams_and_compares_case_insensitively() {
        let dir = std::env::temp_dir().join(format!("odm-checksum-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("blob.bin");
        std::fs::write(&path, b"hello world").unwrap();
        let p = path.to_str().unwrap();
        let sha256 = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9";
        verify_checksum(p, "sha256", sha256).unwrap();
        verify_checksum(p, "SHA256", &sha256.to_uppercase()).unwrap();
        verify_checksum(p, "sha1", "2aae6c35c94fcfb415dbe95f408b9ce91ee846ed").unwrap();
        assert!(verify_checksum(p, "sha256", &"0".repeat(64)).is_err());
        let _ = std::fs::remove_file(path);
    }
}

/// Exit codes from internal/download/manager.go (manager.go consts).
pub mod exit_code {
    pub const EXIT_OK: i32 = 0;
    pub const EXIT_GENERAL: i32 = 1;
    pub const EXIT_NETWORK: i32 = 2;
    pub const EXIT_PARTIAL: i32 = 3;
    pub const EXIT_CANCELLED: i32 = 4;
}

/// `exit_code_from` counts succeeded/failed/cancelled to produce the right code.
pub fn exit_code_from(succeeded: usize, failed: usize, cancelled: usize) -> i32 {
    use exit_code::*;
    if cancelled > 0 {
        EXIT_CANCELLED
    } else if failed == 0 && succeeded == 0 {
        EXIT_GENERAL
    } else if failed == 0 {
        EXIT_OK
    } else if succeeded == 0 {
        EXIT_NETWORK
    } else {
        EXIT_PARTIAL
    }
}
