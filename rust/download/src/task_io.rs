//! Rust port of ODM's `internal/download/task_io.go` — worker loop,
//! `download_chunk` (retry/backoff), `fetch_and_write`/`fetch_whole`/`copy_stream`.

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use futures_util::{Stream, StreamExt};
use transport::{self, StatusError};

use crate::chunkqueue::Chunk;
use crate::csha256::FastSha256;
use crate::engine::Engine;
use crate::task::{is_permanent, status_err, Error, Task, TaskState, Ctx};

/// A rate-limit wait must abort on ^C (Go's `WaitN(ctx, …)`); without the ctx
/// in the select, a low `--limit-rate` could stall shutdown until refill.
async fn rate_acquire(lim: &ratelimit::Limiter, n: i64, ctx: &Ctx) -> Result<(), Error> {
    tokio::select! {
        r = lim.acquire_async(n) => r.map_err(Error::msg),
        _ = ctx.wait() => Err(Error::msg("cancelled during rate-limit wait")),
    }
}

/// `hex_encode` — sha256 → lowercase hex (matches Go hex.EncodeToString).
fn hex_encode(h: FastSha256) -> String {
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

impl Task {
    /// `worker` pulls chunks from the given engine's queue and downloads
    /// them with retry on transient failures.
    pub async fn worker(self: &Arc<Self>, ctx: Ctx, eng: Engine) {
        loop {
            // Graceful drain (AdjustConns reduction).
            if self.retire_if_above_target() {
                return;
            }
            self.pause_gate(&ctx).await;
            if ctx.is_cancelled() {
                return;
            }
            let Some(c) = eng.next() else { return };
            if let Err(e) = self.download_chunk(&ctx, &eng, c).await {
                if ctx.is_cancelled() {
                    self.errors.fetch_add(1, Ordering::SeqCst);
                    self.persist_control();
                    return;
                }
                if is_permanent(&e) {
                    self.errors.fetch_add(1, Ordering::SeqCst);
                    self.set_state(TaskState::Error);
                    self.logf("error", format!("chunk {} (off {}) failed permanently: {}", c.index, c.start, e));
                    return;
                }
                if eng.requeue(c, self.opts_retry()) {
                    self.logf("warn", format!("chunk {} (off {}) failed, requeued for retry: {}", c.index, c.start, e));
                    continue;
                }
                self.errors.fetch_add(1, Ordering::SeqCst);
                self.set_state(TaskState::Error);
                self.logf("error", format!("chunk {} (off {}) failed permanently: {}", c.index, c.start, e));
                return;
            }
            eng.mark_done(c);
            if let Some(sink) = self.sink_snapshot() {
                sink(self.snapshot());
            }
            if self.checkpoint() {
                self.persist_control();
            }
        }
    }

    fn opts_retry(&self) -> i32 {
        self.opts.lock().unwrap().retry
    }

    /// `retire_if_above_target` — CAS drain on AdjustConns reduction.
    fn retire_if_above_target(&self) -> bool {
        loop {
            let live = self.conns.load(Ordering::SeqCst);
            let target = self.conn_target.load(Ordering::SeqCst);
            if live <= target {
                return false;
            }
            if self.conns.compare_exchange(live, live - 1, Ordering::SeqCst, Ordering::SeqCst).is_ok() {
                return true;
            }
        }
    }

    /// `throttle_ok` — restore the configured rate after the global cooldown.
    pub fn throttle_ok(&self) {
        self.lim.throttle_ok();
    }

    /// `chunk_timeout_ctx` — spawn a canceller (Go: context.WithTimeout).
    fn chunk_timeout_ctx(&self, ctx: &Ctx) -> (Ctx, tokio::task::JoinHandle<()>) {
        let tmo = self.opts.lock().unwrap().timeout;
        let tmo = if tmo.is_zero() { Duration::from_secs(300) } else { tmo * 10 };
        let h = tokio::spawn({
            let c = ctx.clone();
            async move {
                tokio::time::sleep(tmo).await;
                c.cancel();
            }
        });
        (ctx.clone(), h)
    }

    /// `download_chunk` — fetch + write one chunk with per-attempt retry.
    async fn download_chunk(&self, ctx: &Ctx, eng: &Engine, c: Chunk) -> Result<(), Error> {
        let retry = self.opts_retry();
        let mut last_err = Error::msg("no error");
        for attempt in 0..(retry + 1) {
            if attempt > 0 {
                self.set_state(TaskState::Retrying);
                self.retries.fetch_add(1, Ordering::SeqCst);
                // Exponential backoff, capped at 30s (Go: RetryWait << min(attempt,30)).
                let rw = self.opts.lock().unwrap().retry_wait;
                let shift = (attempt as u32).min(30);
                let wait_nanos = rw.as_nanos() << shift;
                let wait = if rw > Duration::ZERO
                    && (wait_nanos > Duration::from_secs(30).as_nanos() || wait_nanos == 0)
                {
                    Duration::from_secs(30)
                } else {
                    Duration::from_nanos(wait_nanos as u64)
                };
                tokio::select! {
                    _ = ctx.wait() => return Err(Error::msg("context canceled")),
                    _ = tokio::time::sleep(wait) => {}
                }
            }
            self.set_state(TaskState::Active);
            let mut h = FastSha256::new();
            let mut attempt_done: i64 = 0;
            let err = self.fetch_and_write(ctx, eng, c, &mut h, &mut attempt_done).await;
            if err.is_none() {
                let sum = hex_encode(h);
                self.store_chunk_hash(eng.abs_start(c.start), sum);
                self.throttle_ok();
                return Ok(());
            }
            let err = err.unwrap();
            if ctx.is_cancelled() {
                return Err(Error::msg("context canceled"));
            }
            // Roll back this attempt's partial bytes (see Go comment).
            if attempt_done > 0 {
                self.bytes_done.fetch_sub(attempt_done, Ordering::SeqCst);
                if let Some(sink) = self.sink_snapshot() {
                    sink(self.snapshot());
                }
            }
            // 429 → adaptive halving of the global rate.
            if err.status == Some(429) && self.lim.back_off_signal() {
                self.logf("warn", "server asked to slow down (429) — global rate halved");
            }
            // Permanent failures won't heal — stop trying.
            if is_permanent(&err) {
                return Err(err);
            }
            last_err = err;
            self.logf("warn", format!("chunk {} attempt {}: {}", c.index, attempt, last_err));
        }
        Err(last_err)
    }

    /// `fetch_and_write` — a single ranged GET; copies the body to disk.
    async fn fetch_and_write(
        &self,
        ctx: &Ctx,
        eng: &Engine,
        c: Chunk,
        h: &mut FastSha256,
        attempt_done: &mut i64,
    ) -> Option<Error> {
        let (chunk_ctx, timer) = self.chunk_timeout_ctx(ctx);
        let _ = &chunk_ctx; // Go: per-chunk WithTimeout ctx — wired via fetch_whole
        let _ = timer; // Go: cancel after chunk completes — see Go comment.


        let (final_url, total_size, single_stream) = {
            let probe = self.probe.lock().unwrap();
            match probe.as_ref() {
                Some(pr) => (pr.final_url.clone(), pr.total_size, pr.single_stream),
                None => return Some(Error::msg("no probe result")),
            }
        };

        // Sizeless single-stream chunk: plain GET, no Range.
        if total_size < 0 || (single_stream && c.start == 0 && c.index == 0) {
            let r = self.fetch_whole(&chunk_ctx, &final_url, h, attempt_done).await;
            timer.abort();
            return r;
        }

        // Absolute offsets: region2 starts at base.
        let abs_start = eng.abs_start(c.start);
        let abs_end = if c.end >= 0 { eng.abs_start(c.end) } else { -1 };

        // Mirror rotation: each chunk takes the next URL in the mirror list
        // (round-robin). The primary stays in the rotation (slot 0).
        let mut url = final_url.clone();
        let mut from_mirror = false;
        let mirrors = self.opts.lock().unwrap().mirrors.clone();
        if !mirrors.is_empty() {
            let i = (self.mirror_idx.fetch_add(1, Ordering::SeqCst) + 1) as usize % (mirrors.len() + 1);
            if i > 0 {
                url = mirrors[i - 1].clone();
                // Mirrors get no If-Range.
                from_mirror = true;
                self.logf("info", format!("chunk {} from mirror {}", c.index, url));
            }
        }

        let resume_etag = if from_mirror {
            String::new()
        } else {
            self.resume_etag.lock().unwrap().clone()
        };
        let resp = match eng.client().get_range(&url, abs_start, abs_end, &resume_etag).await {
            Ok(r) => r,
            Err(e) => {
                return Some(match e {
                    transport::GetRangeError::Status(se) => Error::from(se),
                    transport::GetRangeError::Transport(m) => Error::msg(m),
                })
            }
        };
        let status = resp.status().as_u16();
        if status != 206 {
            let _ = resp.bytes().await;
            let op = if !self.resume_etag.lock().unwrap().is_empty() {
                "resume target changed since last run (If-Range mismatch)"
            } else {
                "server ignored range request"
            };
            let _ = abs_end;
            return Some(StatusError {
                err: format!("{op} at offset {abs_start} (status {status})"),
                status,
                permanent: true,
            }
            .into());
        }
        let cr = resp.headers().get("Content-Range").and_then(|v| v.to_str().ok()).unwrap_or("").to_string();
        if !content_range_starts_with(&cr, abs_start) {
            let _ = resp.bytes().await;
            return Some(StatusError {
                err: format!("server sent wrong range for offset {abs_start} (Content-Range {cr:?})"),
                status: 417,
                permanent: true,
            }
            .into());
        }

        let mut off: i64 = 0;
        let r = self.copy_stream(ctx, resp.bytes_stream(), eng, c, h, attempt_done, &mut off).await;
        timer.abort();
        if let Err(e) = r {
            return Some(e);
        }
        if c.end >= 0 && total_size > 0 && off != (c.end - c.start + 1) {
            let want = c.end - c.start + 1;
            return Some(StatusError {
                err: format!("chunk {} short read: got {off} want {want}", c.index),
                status: 417,
                permanent: true,
            }
            .into());
        }
        None
    }

    /// `fetch_whole` — single-stream GET without Range (fetchAndWrite).
    async fn fetch_whole(&self, ctx: &Ctx, url: &str, h: &mut FastSha256, attempt_done: &mut i64) -> Option<Error> {
        let (_chunk_ctx, timer) = self.chunk_timeout_ctx(ctx);
        let total_size = self.probe.lock().unwrap().as_ref().map(|p| p.total_size).unwrap_or(-1);
        let rb = self.client.new_get_request(url);
        let resp = match rb.send().await {
            Ok(r) => r,
            Err(e) => return Some(Error::msg(e.to_string())),
        };
        let status = resp.status().as_u16();
        if !(200..300).contains(&status) {
            let _ = resp.bytes().await;
            timer.abort();
            return Some(status_err(format!("GET {url}"), status));
        }
        let mut off: i64 = 0;
        // Go passes base=0 here (fetchWhole ignores the region base).
        let dummy_eng = Engine::new(Arc::new(crate::chunkqueue::ChunkQueue::new(0, 0)), self.client.clone(), 0);
        let whole = Chunk { index: 0, start: 0, end: -1 };
        let r = self.copy_stream(ctx, resp.bytes_stream(), &dummy_eng, whole, h, attempt_done, &mut off).await;
        let _ = &whole; // ensure whole is consumed by copy_stream above
        timer.abort();
        if let Err(e) = r {
            return Some(e);
        }
        if total_size > 0 && off != total_size {
            return Some(StatusError {
                err: format!("short read: got {off} bytes, want {total_size}"),
                status: 417,
                permanent: true,
            }
            .into());
        }
        None
    }

    /// `copy_stream` — copies the stream to disk with rate limiting + hashing.
    async fn copy_stream(
        &self,
        ctx: &Ctx,
        mut stream: impl Stream<Item = Result<Bytes, reqwest::Error>> + Unpin,
        eng: &Engine,
        c: Chunk,
        h: &mut FastSha256,
        attempt_done: &mut i64,
        off: &mut i64,
    ) -> Result<(), Error> {
        // Capture the file and per-task limiter ONCE. Holding the `disk`
        // mutex across every 8 KiB hyper frame serialised all workers and
        // blocked tokio threads; positioned writes are already non-overlap.
        let disk = self.disk.lock().unwrap().clone();
        let tl = self.task_lim.lock().unwrap().clone();
        const WRITE_BUF: usize = 64 * 1024; // Go's copyChunkFrom copy buffer
        let mut buf: Vec<u8> = Vec::with_capacity(WRITE_BUF);

        macro_rules! flush_buf {
            () => {{
                if !buf.is_empty() {
                    let n = buf.len() as i64;
                    if let Some(d) = disk.as_ref() {
                        d.write_at(&buf, eng.abs_start(c.start) + *off)
                            .map_err(|e| Error::msg(format!("write: {e}")))?;
                    }
                    h.update(&buf);
                    *off += n;
                    *attempt_done += n;
                    self.note_bytes(n);
                    buf.clear();
                }
            }};
        }

        while let Some(item) = stream.next().await {
            let data = item.map_err(|e| Error::msg(e.to_string()))?;
            if data.is_empty() {
                continue;
            }
            let delta = data.len() as i64;
            // Rate limiting stays per-frame (Go Acquire before each copy).
            if !self.lim.unlimited() {
                rate_acquire(&self.lim, delta, ctx).await?;
            }
            if let Some(tl) = tl.as_ref() {
                if !tl.unlimited() {
                    rate_acquire(tl, delta, ctx).await?;
                }
            }
            buf.extend_from_slice(&data);
            if buf.len() >= WRITE_BUF {
                flush_buf!();
            }
        }
        flush_buf!();
        Ok(())
    }

    /// `note_bytes` — adds bytes to progress + rolling-rate measure.
    fn note_bytes(&self, delta: i64) {
        self.bytes_done.fetch_add(delta, Ordering::SeqCst);
        let (flushed, speed) = {
            let mut rm = self.rm.lock().unwrap();
            let flushed = rm.tick(delta);
            let speed = rm.bps;
            self.speed.store(speed, Ordering::SeqCst);
            (flushed, speed)
        };
        let _ = speed;
        let _ = speed;
        if flushed {
            if let Some(sink) = self.sink_snapshot() {
                sink(self.snapshot());
            }
        }
    }
}

/// `content_range_starts_with` — Content-Range "bytes S-E/T" starts at `want`.
fn content_range_starts_with(cr: &str, want: i64) -> bool {
    let Some(rest) = cr.strip_prefix("bytes ") else { return false };
    let start_str = rest.split('-').next().unwrap_or("");
    match start_str.parse::<i64>() {
        Ok(s) => s == want,
        Err(_) => false,
    }
}

