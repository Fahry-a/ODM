//! Rust port of ODM's `internal/download/resume_impl.go` — resume/checkpoint/
//! integrity half of Task: control-file persistence, the sampled
//! server-side compare, per-chunk SHA-256 bookkeeping and verification on
//! --continue.

use std::collections::HashMap;
use std::sync::atomic::Ordering;

use crate::chunkqueue::Chunk;
use crate::csha256::FastSha256;
use crate::task::{Error, Task, Ctx, RESUME_PROBE_LEN, RESUME_VERIFY_CHUNKS};

fn hex_of(h: FastSha256) -> String {
    h.finalize().iter().map(|b| format!("{b:02x}")).collect()
}

impl Task {
    /// `resume_sample_spans` — completed spans for the sampled server-side
    /// compare, with absolute offsets and per-region chunk size.
    fn resume_sample_spans(&self, n: usize) -> Vec<Chunk> {
        let pr = self.probe.lock().unwrap().clone().unwrap_or_default();
        // region2_chunk_size() locks `engines` itself — compute it BEFORE
        // taking that guard (std Mutex is not reentrant).
        let seg2 = self.region2_chunk_size();
        let engines = self.engines.lock().unwrap();
        if engines.is_empty() {
            drop(engines);
            let cs = self.layout_chunk_size(0);
            let cs = if cs < 1 { self.options().chunk_size } else { cs };
            let queue = self.queue.lock().unwrap().clone();
            return queue
                .as_ref()
                .map(|q| q.completed_spans(cs, pr.total_size, n))
                .unwrap_or_default();
        }
        let mut out: Vec<Chunk> = Vec::new();
        out.extend(engines[0].completed_spans(self.options().chunk_size, self.split_at.load(Ordering::SeqCst), n));
        let seg = if seg2 < 1 { self.options().chunk_size } else { seg2 };
        let split_at = self.split_at.load(Ordering::SeqCst);
        for s in engines[1].completed_spans(seg, pr.total_size - split_at, n) {
            out.push(Chunk { start: split_at + s.start, end: split_at + s.end, index: -1 });
        }
        out
    }

    /// `verify_resumed_chunks` — samples completed chunks and compares them
    /// against the server (ranged GETs).
    async fn verify_resumed_chunks(&self, _ctx: &Ctx) -> Result<(), String> {
        let pr = self.probe.lock().unwrap().clone();
        let pr = match pr {
            Some(p) if !p.single_stream && p.total_size > 0 => p,
            _ => return Ok(()),
        };
        // resume_sample_spans locks engines itself — do NOT hold the lock
        // across the network awaits below (deadlock + non-Send future).
        for s in self.resume_sample_spans(RESUME_VERIFY_CHUNKS) {
            let mut end = s.start + RESUME_PROBE_LEN - 1;
            if end > s.end {
                end = s.end;
            }
            let want = (end - s.start + 1) as usize;
            let cli = {
                let engines = self.engines.lock().unwrap();
                if !engines.is_empty() && s.start >= self.split_at.load(Ordering::SeqCst) {
                    engines[1].client().clone()
                } else {
                    self.client.clone()
                }
            };
            let resp = cli
                .get_range(&pr.final_url, s.start, end, "")
                .await
                .map_err(|e| format!("resume check at {}: {}", s.start, e))?;
            let got = resp.bytes().await.map_err(|e| format!("resume check at {}: {e}", s.start))?;
            if got.len() < want {
                return Err(format!("resume check at {}: EOF (want {want} bytes)", s.start));
            }
            let mut disk_buf = vec![0u8; want];
            {
                let disk = self.disk.lock().unwrap();
                disk.as_ref()
                    .unwrap()
                    .read_at(&mut disk_buf, s.start)
                    .map_err(|e| format!("resume check at {}: {e}", s.start))?;
            }
            if disk_buf != got[..want] {
                return Err(format!("resume check at {}: on-disk data differs from server", s.start));
            }
        }
        Ok(())
    }

    /// `store_chunk_hash` — record the SHA-256 of a completed chunk by its
    /// Start offset (guarded by the same mutex persist_control uses).
    pub(crate) fn store_chunk_hash(&self, start: i64, sum: String) {
        self.chunk_hashes.lock().unwrap().insert(start, sum);
    }

    /// `clear_chunk_hashes` — used when a resume integrity check fails and
    /// the download restarts from scratch.
    pub(crate) fn clear_chunk_hashes(&self) {
        *self.chunk_hashes.lock().unwrap() = HashMap::new();
    }

    /// `restore_chunk_hashes` — seed the in-memory hash map from a control
    /// file being resumed, so hashes survive into subsequent checkpoints.
    pub(crate) fn restore_chunk_hashes(&self, cf: &storage::ControlFile) {
        if cf.chunk_hashes.as_ref().map_or(true, |m| m.is_empty()) {
            return;
        }
        let mut hashes = self.chunk_hashes.lock().unwrap();
        if self.engines.lock().unwrap().is_empty() {
            if let Some(q) = self.queue.lock().unwrap().as_ref() {
                for off in q.completed_offsets() {
                    if let Some(sum) = cf.chunk_hash(off) {
                        hashes.insert(off, sum);
                    }
                }
            }
            return;
        }
        let engines = self.engines.lock().unwrap();
        for off in engines[0].completed_offsets() {
            if let Some(sum) = cf.chunk_hash(off) {
                hashes.insert(off, sum);
            }
        }
        for off in engines[1].completed_offsets() {
            let abs = off + engines[1].base;
            if let Some(sum) = cf.chunk_hash(abs) {
                hashes.insert(abs, sum);
            }
        }
    }

    /// `snapshot_chunk_hashes` — the recorded hashes for the given completed
    /// offsets (subset of chunk_hashes for offsets actually MarkDone'd).
    pub(crate) fn snapshot_chunk_hashes(&self, completed: &[i64]) -> Option<HashMap<i64, String>> {
        let hashes = self.chunk_hashes.lock().unwrap();
        if hashes.is_empty() {
            return None;
        }
        Some(completed.iter().filter_map(|off| hashes.get(off).cloned().map(|s| (*off, s))).collect())
    }

    /// `verify_resumed_data` — composes local-disk per-chunk hashes + the
    /// sampled server-side compare. Either failure → re-download from scratch.
    pub async fn verify_resumed_data(&self, ctx: &Ctx, cf: Option<&storage::ControlFile>) -> Result<(), Error> {
        if self.has_full_hash_coverage(cf) {
            self.verify_resumed_hashes(cf.unwrap())?;
        }
        self.verify_resumed_chunks(ctx).await.map_err(Error::msg)
    }

    /// `has_full_hash_coverage` — every completed chunk in cf has a hash.
    fn has_full_hash_coverage(&self, cf: Option<&storage::ControlFile>) -> bool {
        let Some(cf) = cf else { return false };
        if cf.chunk_hashes.as_ref().map_or(true, |m| m.is_empty()) {
            return false;
        }
        cf.completed_offsets().iter().all(|off| cf.chunk_hash(*off).is_some())
    }

    /// `verify_resumed_hashes` — per-chunk SHA-256 of on-disk bytes
    /// (LOCAL corruption check). One mismatch fails the resume.
    fn verify_resumed_hashes(&self, cf: &storage::ControlFile) -> Result<(), Error> {
        let pr = self.probe.lock().unwrap().clone().unwrap_or_default();
        if pr.total_size <= 0 {
            return Ok(());
        }
        let mut starts: Vec<i64> = cf.completed_offsets().into_iter().collect();
        starts.sort_unstable();
        let disk = self.disk.lock().unwrap();
        let disk = disk.as_ref().unwrap();
        let mut buf = vec![0u8; 65536];
        for off in starts {
            let Some(want) = cf.chunk_hash(off) else {
                return Err(Error::msg(format!("resume hash check at {off}: no recorded hash for completed chunk")));
            };
            let (start, end) = self.chunk_span(off);
            if end < start {
                return Err(Error::msg(format!("resume hash check at {off}: invalid span [{start}, {end}]")));
            }
            let mut h = FastSha256::new();
            let mut pos = start;
            while pos <= end {
                let n = ((end - pos + 1) as usize).min(buf.len()) as i64;
                let b = &mut buf[..n as usize];
                if let Err(e) = disk.read_at(b, pos) {
                    return Err(Error::msg(format!("resume hash check at {off}: read: {e}")));
                }
                h.update(&buf[..n as usize]);
                pos += n;
            }
            if hex_of(h) != want {
                return Err(Error::msg(format!("resume hash check at {off}: on-disk data differs from recorded hash")));
            }
        }
        Ok(())
    }

    /// `chunk_span` — the byte span [start, end] a chunk beginning at `start`
    /// occupies in the current layout.
    fn chunk_span(&self, start: i64) -> (i64, i64) {
        let pr = self.probe.lock().unwrap().clone().unwrap_or_default();
        if pr.single_stream {
            return (0, pr.total_size - 1);
        }
        let cs = self.layout_chunk_size(start);
        let mut end = start + cs - 1;
        if pr.total_size > 0 && end >= pr.total_size {
            end = pr.total_size - 1;
        }
        (start, end)
    }

    /// `layout_chunk_size` — size of the chunk/segment beginning at `start`
    /// in the CURRENT layout (opts.ChunkSize for odm, aria2c segment size for
    /// aria2c/both-region2).
    fn layout_chunk_size(&self, start: i64) -> i64 {
        // Compute this BEFORE holding `engines` — region2_chunk_size() locks
        // the same non-reentrant mutex (same deadlock as persist_control).
        let seg2 = self.region2_chunk_size();
        let engines = self.engines.lock().unwrap();
        let split_at = self.split_at.load(Ordering::SeqCst);
        if !engines.is_empty() && start >= split_at && seg2 > 0 {
            return seg2;
        }
        drop(engines);
        if self.options().profile == "aria2c" {
            let seg = self.effective_chunk_size();
            if seg > 0 {
                return seg;
            }
        }
        let cs = self.options().chunk_size;
        if cs < 1 {
            crate::chunkqueue::DEFAULT_CHUNK_SIZE
        } else {
            cs
        }
    }
}
