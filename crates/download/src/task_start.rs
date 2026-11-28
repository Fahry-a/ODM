//! Rust port of ODM's `internal/download/task_start.go` — `Task::start`.

use std::path::Path;
use std::sync::atomic::Ordering;
use std::sync::Arc;

use crate::chunkqueue::{ChunkQueue, StaticQueue, WorkQueue};
use crate::engine::{aria_split, Engine};
use crate::smart::{choose_profile, ServerCapabilities};
use crate::task::{derive_filename, flatten_filename, format_seg_size, size_or_unknown, Error, Sink, Task, TaskState, Ctx};
use crate::task_filename::unique_name;
use std::collections::HashSet;

impl Task {
    /// `start` runs Probe → open file → start workers. Blocks until the task
    /// finishes (completed or errored) or ctx is cancelled.
    pub async fn start(self: &Arc<Self>, parent_ctx: &Ctx, mut conns: i32, progress_sink: Option<Sink>) -> Result<(), Error> {
        let ctx = Ctx::with_parent(parent_ctx);
        self.cancel_ctx.lock().unwrap().replace(ctx.clone());
        self.base_ctx.lock().unwrap().replace(ctx.clone());
        *self.sink.lock().unwrap() = progress_sink.clone();

        // Fail fast if RPC Cancel fired while the task was still queued: there
        // was no cancel ctx to signal then. Mirrors Go's Start() entry check.
        if self.cancelled.load(Ordering::SeqCst) {
            ctx.cancel();
            self.set_state(TaskState::Error);
            self.emit_final(progress_sink.clone());
            return Err(Error::msg("task cancelled before start"));
        }

        self.set_state(TaskState::Active);

        // 1. Probe. The CLI one-shot path already probed every URL and
        // injects the result via SetProbe, so a fresh network probe is
        // skipped there. The RPC daemon path leaves it nil and probes here.
        // Guards are dropped before any .await (the future must be Send).
        let mut pr = self.probe.lock().unwrap().clone();
        if pr.is_none() {
            self.logf("info", format!("probing {}", self.url));
            let pr2 = match self.client.probe(&self.url).await {
                Ok(pr) => pr,
                Err(e) => {
                    self.set_state(TaskState::Error);
                    self.emit_final(progress_sink.clone());
                    return Err(Error::msg(format!("probe: {e}")));
                }
            };
            *self.probe.lock().unwrap() = Some(pr2.clone());
            pr = Some(pr2);
        }
        let mut pr = pr.unwrap();

        // --checksum-url: pull the digest from the sidecar before any byte of
        // the payload moves. A fetch or parse failure fails the task.
        let cs_opts = self.options().clone();
        if !cs_opts.checksum_url.is_empty() && cs_opts.checksum.is_empty() {
            match self.fetch_checksum_spec(&ctx).await {
                Ok(spec) => {
                    self.options().checksum = spec.clone();
                    self.logf("info", format!("checksum from {}: {}", self.options().checksum_url, spec));
                }
                Err(e) => {
                    self.set_state(TaskState::Error);
                    self.emit_final(progress_sink.clone());
                    return Err(Error::msg(format!("checksum-url: {e}")));
                }
            }
        }

        // Smart profile: decide the concrete engine now that the probe
        // answered range support + size.
        if self.options().profile == "smart" {
            let per_file = {
                let c = self.conns.load(Ordering::SeqCst);
                if c >= 1 { c } else { conns }
            };
            let h2c = self.h2_client.lock().unwrap().clone();
            let h2_ready = match h2c.as_ref() {
                Some(c) => c.supports_http2(&self.url).await,
                None => false,
            };
            let (profile, reason) = choose_profile(ServerCapabilities {
                total_size: pr.total_size,
                supports_range: pr.supports_range,
                single_stream: pr.single_stream,
                http2_ready: h2_ready,
                conns: per_file,
            });
            self.logf("info", format!("smart profile: chose {profile:?} ({reason})"));
            self.options().profile = profile.to_string();
        }

        // Filename refinement: publish a COPIED probe (Go comment).
        {
            let mut probe_g = self.probe.lock().unwrap();
            if let Some(p) = probe_g.as_mut() {
                if p.filename.is_empty() || !self.options().output_name.is_empty() {
                    if p.filename.is_empty() {
                        p.filename = derive_filename(&p.final_url, &self.options().output_name);
                    }
                    if !self.options().output_name.is_empty() {
                        p.filename = self.options().output_name.clone(); // explicit -o wins
                    }
                }
            }
            pr = probe_g.clone().unwrap();
        }
        self.set_state(TaskState::Active);

        // 2. Resolve paths + attempt resume.
        let dir = self.options().dir.clone();
        let mut out_name = flatten_filename(&pr.filename);
        if out_name.is_empty() {
            out_name = "download.bin".to_string();
        }
        *self.out_path.lock().unwrap() = Path::new(&dir).join(&out_name).to_string_lossy().into_owned();

        // Collision policy — applies only when this run is NOT resuming.
        let resumable = {
            self.options().continue_ && storage::load_control(&self.out_path.lock().unwrap()).is_ok()
        };
        if !resumable {
            match self.options().collision.as_str() {
                "skip" => {
                    let st = std::fs::metadata(&self.out_path.lock().unwrap().clone()).ok().filter(|m| m.is_file());
                    if let Some(st) = st {
                        if pr.total_size > 0 && st.len() == pr.total_size as u64 {
                            self.logf("info", format!("skipping {out_name}: already downloaded ({} bytes)", st.len()));
                            self.bytes_done.store(st.len() as i64, Ordering::SeqCst);
                            self.set_state(TaskState::Completed);
                            self.emit_final(progress_sink.clone());
                            return Ok(());
                        }
                        self.logf(
                            "warn",
                            format!("--skip-existing: {out_name} exists with a different size ({} ≠ {}), re-downloading", st.len(), size_or_unknown(pr.total_size)),
                        );
                    }
                }
                "rename" => {
                    if std::fs::metadata(&self.out_path.lock().unwrap().clone()).is_ok() {
                        let new_name = unique_name(&dir, &out_name);
                        self.logf("info", format!("{out_name} exists — saving as {new_name}"));
                        {
                            let mut probe_g = self.probe.lock().unwrap();
                            if let Some(p) = probe_g.as_mut() {
                                p.filename = new_name.clone();
                            }
                        }
                        pr.filename = new_name.clone();
                        // open_for_write below reuses the local `out_name`; if
                        // it stays the old name the rename is immediately undone.
                        out_name = new_name;
                        *self.out_path.lock().unwrap() = Path::new(&dir).join(&out_name).to_string_lossy().into_owned();
                    }
                }
                _ => {}
            }
        }

        // Probe-derived size check before opening: SingleStream forces
        // qs = -1 so the queue holds one whole-file chunk.
        let qs = if pr.single_stream { -1 } else { pr.total_size };
        let opts_profile = self.options().profile.clone();
        let opts_chunk_size = self.options().chunk_size;
        let opts_split = self.options().split as i64;
        let opts_min_split = self.options().min_split_size;
        let is_aria = opts_profile == "aria2c";
        let mut is_both = opts_profile == "both";
        {
            let mut engines = self.engines.lock().unwrap();
            engines.clear();
            *self.single.lock().unwrap() = None;
            self.split_at.store(0, Ordering::SeqCst);
        }
        let mut effective = opts_chunk_size;
        if is_aria && qs > 0 {
            let (n, seg) = aria_split(qs, opts_split, opts_min_split);
            effective = seg;
            self.aria_split.store(n, Ordering::SeqCst);
            self.logf("info", format!("aria2c profile: {n} segments of ~{} each", format_seg_size(seg)));
        }
        if is_both && qs > 0 && qs < 4 * 1024 * 1024 {
            is_both = false;
            self.logf("info", "both profile: file < 4 MiB, using single-region odm engine");
        }
        if is_both && qs > 0 && conns < 2 {
            is_both = false;
            self.logf("info", format!("both profile: {conns} connection(s), using single-region odm engine"));
        }
        let mut split_at: i64 = 0;
        if is_both && qs > 0 {
            let conns1 = std::cmp::max(1, conns / 2);
            let conns2 = std::cmp::max(1, conns - conns1);
            *self.region_conns.lock().unwrap() = vec![conns1, conns2];
            let mid = if qs / 2 < 1 { 1 } else { qs / 2 };
            split_at = mid;
            let (n2, _) = aria_split(qs - mid, opts_split, opts_min_split);
            let q1: Arc<dyn WorkQueue> = Arc::new(ChunkQueue::new(mid, opts_chunk_size));
            let q2: Arc<dyn WorkQueue> = Arc::new(StaticQueue::new(qs - mid, n2));
            *self.engines.lock().unwrap() =
                vec![Engine::new(q1, self.client.clone(), 0), Engine::new(q2, self.engine_client(true), mid)];
            self.aria_split.store(n2, Ordering::SeqCst);
            self.logf(
                "info",
                format!("both profile: region1 [0,{mid}) odm ({conns1} conns, h1), region2 [{mid},{qs}) aria2c ({n2} segments, h2)"),
            );
        }
        self.split_at.store(split_at, Ordering::SeqCst);
        let q: Option<Arc<dyn WorkQueue>>;
        if !self.engines.lock().unwrap().is_empty() {
            let engines = self.engines.lock().unwrap();
            q = engines.first().map(|e| e.q.clone());
        } else if is_aria && qs > 0 {
            q = Some(Arc::new(StaticQueue::new(qs, self.aria_split.load(Ordering::SeqCst))));
        } else {
            q = Some(Arc::new(ChunkQueue::new(qs, effective)));
        }
        *self.queue.lock().unwrap() = q.clone();

        // 3. Resume.
        let mut already_done: i64 = 0;
        let mut control_file: Option<storage::ControlFile> = None;
        if self.options().continue_ {
            if let Ok(cf) = storage::load_control(&self.out_path.lock().unwrap()) {
                control_file = Some(cf.clone());
                let etag_changed = cf.etag.is_some() && !pr.etag.is_empty() && cf.etag.as_deref() != Some(pr.etag.as_str());
                let layout_match = cf.total_size == pr.total_size
                    && cf.chunk_size == effective
                    && (cf.profile.as_deref().unwrap_or("").is_empty()
                        || cf.profile.as_deref() == Some(self.options().profile.as_str()))
                    && cf.split_at == Some(split_at)
                    && cf.region2_chunk_size == Some(self.region2_chunk_size());
                if etag_changed {
                    self.logf(
                        "warn",
                        format!("ETag changed ({} → {}), re-downloading from scratch", cf.etag.clone().unwrap_or_default(), pr.etag),
                    );
                } else if layout_match {
                    let mut ok = false;
                    let offs = cf.completed_offsets();
                    let engines = self.engines.lock().unwrap();
                    if !engines.is_empty() {
                        let mut done1: HashSet<i64> = HashSet::new();
                        let mut done2: HashSet<i64> = HashSet::new();
                        for off in offs {
                            if off >= split_at {
                                done2.insert(off - split_at);
                            } else {
                                done1.insert(off);
                            }
                        }
                        let (a1, ok1) = engines[0].reset_completed_offsets(&done1, split_at);
                        let (a2, _) = engines[1].reset_completed_offsets(&done2, engines[1].base);
                        already_done = a1 + a2;
                        ok = ok1;
                    } else {
                        drop(engines);
                        if let Some(q) = self.queue.lock().unwrap().as_ref() {
                            let (a, ok2) = q.reset_completed_offsets(&offs, pr.total_size);
                            already_done = a;
                            ok = ok2;
                        }
                    }
                    if !ok {
                        self.logf("warn", "control file layout doesn't match this download, re-downloading from scratch");
                        already_done = 0;
                    } else {
                        // Pin the control file's ETag for If-Range on every
                        // ranged GET this run (empty when the server never
                        // sent one).
                        *self.resume_etag.lock().unwrap() = cf.etag.clone().unwrap_or_default();
                        self.bytes_done.store(already_done, Ordering::SeqCst);
                        self.restore_chunk_hashes(&cf);
                        self.logf("info", format!("resuming {out_name}: {already_done} bytes already written"));
                    }
                }
            }
        }

        let disk = storage::File::open_for_write(&dir, &out_name, pr.total_size).map_err(Error::from)?;
        *self.out_path.lock().unwrap() = Path::new(&dir).join(&out_name).to_string_lossy().into_owned();
        *self.disk.lock().unwrap() = Some(Arc::new(disk));

        // Resume integrity check.
        if already_done > 0 {
            if let Err(e) = self.verify_resumed_data(&ctx, control_file.as_ref()).await {
                self.logf("warn", format!("resume integrity check failed ({e}) — re-downloading from scratch"));
                already_done = 0;
                self.bytes_done.store(0, Ordering::SeqCst);
                self.clear_chunk_hashes();
                if !self.engines.lock().unwrap().is_empty() {
                    let mid = self.split_at.load(Ordering::SeqCst);
                    let (n2, _) = aria_split(qs - mid, opts_split, opts_min_split);
                    let q1: Arc<dyn WorkQueue> = Arc::new(ChunkQueue::new(mid, opts_chunk_size));
                    let q2: Arc<dyn WorkQueue> = Arc::new(StaticQueue::new(qs - mid, n2));
                    *self.engines.lock().unwrap() =
                        vec![Engine::new(q1, self.client.clone(), 0), Engine::new(q2, self.engine_client(true), mid)];
                    *self.queue.lock().unwrap() = engines_first_queue(self);
                } else if is_aria && qs > 0 {
                    *self.queue.lock().unwrap() = Some(Arc::new(StaticQueue::new(qs, self.aria_split.load(Ordering::SeqCst))));
                } else {
                    *self.queue.lock().unwrap() = Some(Arc::new(ChunkQueue::new(qs, effective)));
                }
            }
        }

        if pr.total_size >= 0 && already_done >= pr.total_size && pr.total_size > 0 {
            // Already complete on disk (resume found everything done).
            self.bytes_done.store(pr.total_size, Ordering::SeqCst);
            self.set_state(TaskState::Completed);
            if let Err(e) = self.verify_checksum() {
                self.logf("error", format!("checksum: {e}"));
                self.set_state(TaskState::Error);
                self.persist_control();
                self.emit_final(progress_sink.clone());
                return Err(e);
            }
            self.emit_final(progress_sink.clone());
            return self.finish().map_err(Error::msg);
        }

        // 3b. Launch workers.
        if (self.conn_target.load(Ordering::SeqCst) as i32) > conns {
            conns = self.conn_target.load(Ordering::SeqCst);
        }
        self.conn_target.store(conns as i32, Ordering::SeqCst);
        if pr.total_size <= 0 && pr.single_stream {
            conns = 1;
            self.conns.store(1, Ordering::SeqCst);
            self.conn_target.store(1, Ordering::SeqCst);
        }

        // Write the control file immediately (like aria2's .aria2).
        self.persist_control();

        // aria2c profile: cap workers at split count / MaxConnPerServer.
        let mut worker_count = conns;
        if is_aria {
            if self.aria_split.load(Ordering::SeqCst) > 0 && (worker_count as i64) > self.aria_split.load(Ordering::SeqCst) {
                worker_count = self.aria_split.load(Ordering::SeqCst) as i32;
            }
            let mcps = self.options().max_conn_per_server;
            if pr.total_size > 0 && !self.profile_uses_h2() && mcps > 0 && worker_count > mcps {
                worker_count = mcps;
            }
            conns = worker_count;
        }
        self.conns.store(conns as i32, Ordering::SeqCst);
        self.conn_target.store(conns as i32, Ordering::SeqCst);
        if !self.engines.lock().unwrap().is_empty() {
            let engines = self.engines.lock().unwrap().clone();
            let region_conns = self.region_conns.lock().unwrap().clone();
            for (ei, eng) in engines.iter().enumerate() {
                let n = region_conns[ei];
                for _ in 0..n {
                    let me = self.clone();
                    let eng2 = eng.clone();
                    let ctx2 = ctx.clone();
                    let h = tokio::spawn(async move { me.worker(ctx2, eng2).await });
                    self.worker_wg.lock().unwrap().push(h);
                }
            }
        } else {
            let engine = self.current_engine();
            for _ in 0..worker_count {
                let me = self.clone();
                let engine2 = engine.clone();
                let ctx2 = ctx.clone();
                let h = tokio::spawn(async move { me.worker(ctx2, engine2).await });
                self.worker_wg.lock().unwrap().push(h);
            }
        }
        let handles = self.worker_wg.lock().unwrap().drain(..).collect::<Vec<_>>();
        for h in handles {
            let _ = h.await;
        }
        *self.adjust_done.lock().unwrap() = true;

        if ctx.is_cancelled() {
            self.set_state(TaskState::Paused);
            self.persist_control();
            self.emit_final(progress_sink.clone());
            return Err(Error::msg("cancelled"));
        }
        if self.errors.load(Ordering::SeqCst) > 0 {
            self.set_state(TaskState::Error);
            self.persist_control();
            self.emit_final(progress_sink.clone());
            let e = self.errors.load(Ordering::SeqCst);
            let b = self.bytes_done.load(Ordering::SeqCst);
            let t = self.total_or_done();
            return Err(Error::msg(format!("task failed: {e} chunk errors, {b}/{t} bytes")));
        }
        self.set_state(TaskState::Completed);
        if let Err(e) = self.verify_checksum() {
            self.logf("error", format!("checksum: {e}"));
            self.set_state(TaskState::Error);
            self.persist_control();
            self.emit_final(progress_sink.clone());
            return Err(e);
        }
        self.emit_final(progress_sink.clone());
        self.finish().map_err(Error::msg)
    }
}

fn engines_first_queue(t: &Arc<Task>) -> Option<Arc<dyn WorkQueue>> {
    t.engines.lock().unwrap().first().map(|e| e.q.clone())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::task::TaskOptions;
    use std::path::PathBuf;

    fn client() -> Arc<transport::Client> {
        Arc::new(transport::new_client(transport::ClientConfig::default()).unwrap())
    }
    fn limiter() -> Arc<ratelimit::Limiter> {
        Arc::new(ratelimit::Limiter::default())
    }
    fn test_dir(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("odm-start-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[tokio::test]
    async fn cancel_before_start_fails_fast_without_network() {
        let t = Task::new(
            "odm-1".into(),
            "http://127.0.0.1:1/x.bin",
            TaskOptions::default(),
            client(),
            limiter(),
            None,
        );
        t.cancel();
        let err = t.start(&Ctx::new(), 2, None).await.unwrap_err();
        assert_eq!(err.msg, "task cancelled before start");
        assert_eq!(t.state(), TaskState::Error);
    }

    #[tokio::test]
    async fn checksum_mismatch_never_leaves_task_completed() {
        let dir = test_dir("cs-mismatch");
        let dest = dir.join("f.bin");
        std::fs::write(&dest, vec![0u8; 64]).unwrap();
        // A control file marking the whole file done reaches the
        // already-complete-on-disk branch, where verifyChecksum runs.
        let cf = storage::ControlFile {
            total_size: 64,
            chunk_size: 64,
            completed: Some(vec![0]),
            split_at: Some(0),
            region2_chunk_size: Some(0),
            ..Default::default()
        };
        storage::save_control(dest.to_str().unwrap(), &cf).unwrap();

        let opts = TaskOptions {
            dir: dir.to_str().unwrap().into(),
            output_name: "f.bin".into(),
            continue_: true,
            chunk_size: 64,
            checksum: format!("sha256:{}", "0".repeat(64)),
            ..Default::default()
        };
        let t = Task::new("odm-2".into(), "http://127.0.0.1:1/f.bin", opts, client(), limiter(), None);
        *t.probe.lock().unwrap() = Some(transport::ProbeResult {
            final_url: "http://127.0.0.1:1/f.bin".into(),
            filename: "f.bin".into(),
            total_size: 64,
            single_stream: true,
            ..Default::default()
        });

        let err = t.start(&Ctx::new(), 0, None).await.unwrap_err();
        assert!(err.msg.starts_with("checksum mismatch"), "{err:?}");
        assert_eq!(t.state(), TaskState::Error);
        assert_ne!(t.state(), TaskState::Completed);
    }

    #[tokio::test]
    async fn auto_rename_writes_unique_name_and_preserves_original() {
        let dir = test_dir("auto-rename");
        let original = dir.join("file.bin");
        std::fs::write(&original, b"original").unwrap();

        let opts = TaskOptions {
            dir: dir.to_str().unwrap().into(),
            output_name: "file.bin".into(),
            collision: "rename".into(),
            ..Default::default()
        };
        let t = Task::new("odm-3".into(), "http://127.0.0.1:1/file.bin", opts, client(), limiter(), None);
        *t.probe.lock().unwrap() = Some(transport::ProbeResult {
            final_url: "http://127.0.0.1:1/file.bin".into(),
            filename: "file.bin".into(),
            total_size: 16,
            ..Default::default()
        });

        // conns=0 → no workers/network; the renamed file is still opened.
        let _ = t.start(&Ctx::new(), 0, None).await;
        assert!(dir.join("file.1.bin").exists(), "expected renamed output file.1.bin");
        assert_eq!(std::fs::read(&original).unwrap(), b"original");
    }
}
