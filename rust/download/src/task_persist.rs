//! Rust port of ODM's `internal/download/task_persist.go` — finish,
//! checkpoint, persistControl, effectiveChunkSize, region2ChunkSize.

use std::sync::atomic::Ordering;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use crate::engine::aria_split;
use crate::task::{Task, PERSIST_CHECKPOINT_INTERVAL, PERSIST_MIN_INTERVAL};
use version::VERSION;

/// RFC3339 UTC timestamp for control-file metadata (Go's `time.RFC3339`).
/// Hand-rolled to avoid a date crate; the civil-date conversion is the standard
/// days-from-epoch algorithm and is exact for UNIX seconds.
fn rfc3339_now() -> String {
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    let days = secs.div_euclid(86_400);
    let sod = secs.rem_euclid(86_400);
    let (year, month, day) = civil_from_days(days);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        sod / 3600,
        (sod % 3600) / 60,
        sod % 60,
    )
}

fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (if m <= 2 { y + 1 } else { y }, m as u32, d as u32)
}

impl Task {
    /// `finish` — flush + persist/remove the control file; an error from the
    /// caller already set the state.
    pub fn finish(&self) -> Result<(), String> {
        if let Some(disk) = self.disk.lock().unwrap().as_ref() {
            let _ = disk.sync();
        }
        // On success the control file is removed.
        if self.state() == crate::task::TaskState::Completed {
            let _ = storage::remove_control(&self.output_path());
        } else {
            // Persist what we have so a later --continue can resume.
            self.persist_control();
        }
        Ok(())
    }

    /// `effective_chunk_size` — the chunk size the current layout was built
    /// with (aria2c segment size for the aria2c profile, else opts.ChunkSize).
    pub fn effective_chunk_size(&self) -> i64 {
        // Clone once: re-locking `opts` inside the `if` body while the
        // condition's temporary guard is still alive deadlocks (std Mutex is
        // not reentrant) for the aria2c profile.
        let opts = self.options().clone();
        if opts.profile == "aria2c" && self.aria_split.load(Ordering::SeqCst) > 0 {
            if let Some(pr) = self.probe.lock().unwrap().as_ref() {
                if pr.total_size > 0 {
                    let (_, seg) = aria_split(pr.total_size, opts.split as i64, opts.min_split_size);
                    return seg;
                }
            }
        }
        opts.chunk_size
    }

    /// `region2_chunk_size` — segment size of the both profile's second
    /// engine (0 otherwise).
    pub fn region2_chunk_size(&self) -> i64 {
        let engines = self.engines.lock().unwrap();
        if engines.is_empty() {
            return 0;
        }
        // One `opts` clone: two `self.options()` calls in a single expression
        // hold overlapping MutexGuards and deadlock.
        let opts = self.options().clone();
        if let Some(pr) = self.probe.lock().unwrap().as_ref() {
            if pr.total_size > 0 {
                let (_, seg) = aria_split(
                    pr.total_size - self.split_at.load(Ordering::SeqCst),
                    opts.split as i64,
                    opts.min_split_size,
                );
                return seg;
            }
        }
        0
    }

    /// `checkpoint` — true when the control file should be flushed: either
    /// persistCheckpointInterval chunks completed since the last flush, or
    /// persistMinInterval has elapsed since the last write.
    pub fn checkpoint(&self) -> bool {
        let mut chunks_since_persist = self.chunks_since_persist.lock().unwrap();
        *chunks_since_persist += 1;
        if *chunks_since_persist >= PERSIST_CHECKPOINT_INTERVAL {
            *chunks_since_persist = 0;
            return true;
        }
        match *self.last_persist.lock().unwrap() {
            Some(t) => t.elapsed() >= PERSIST_MIN_INTERVAL,
            None => false,
        }
    }

    /// `persist_control` — records completed chunk offsets + per-chunk
    /// SHA-256 hashes for resume. Mutex-guarded (Go: t.mu).
    pub fn persist_control(&self) {
        let Some(pr) = self.probe.lock().unwrap().as_ref().cloned() else { return };
        if self.queue.lock().unwrap().is_none() {
            return;
        }
        let _guard = self.mu.lock();
        let now = Instant::now();
        // Go writes RFC3339 timestamps; an epoch-seconds string cannot be
        // unmarshalled by the Go binary on a cross-binary resume.
        let now_str = rfc3339_now();
        let mut created = self.control_created_at.lock().unwrap();
        if created.is_none() {
            *created = Some(now_str.clone());
        }
        let created_str = created.clone().unwrap_or_else(|| now_str.clone());
        drop(created);
        *self.last_persist.lock().unwrap() = Some(now);

        // Compute this BEFORE holding `engines` — region2_chunk_size() locks
        // the same mutex (a non-reentrant std Mutex), which would deadlock.
        let region2 = self.region2_chunk_size();

        // Completed offsets are persisted as ABSOLUTE file offsets. For the
        // both profile the second engine's queue is 0-based within its
        // region, so its offsets are translated here (base + rel).
        let engines = self.engines.lock().unwrap();
        let mut completed: Vec<i64> = Vec::new();
        if !engines.is_empty() {
            completed.extend(engines[0].completed_offsets());
            for off in engines[1].completed_offsets() {
                completed.push(off + engines[1].base);
            }
        } else if let Some(q) = self.queue.lock().unwrap().as_ref() {
            completed.extend(q.completed_offsets());
        }
        let op = self.output_path();
        let cf_chunk = self.effective_chunk_size();
        let cf_opts = self.options().clone();
        let cf_hashes = self.snapshot_chunk_hashes(&completed).map(|m| m.into_iter().collect());
        let cf = storage::ControlFile {
            url: self.url.clone(),
            final_url: Some(pr.final_url.clone()),
            total_size: pr.total_size,
            chunk_size: cf_chunk,
            etag: Some(pr.etag.clone()),
            completed: Some(completed.clone()),
            created_at: Some(created_str),
            updated_at: Some(now_str),
            connections: Some(self.conns.load(Ordering::SeqCst) as i64),
            user_agent: Some(cf_opts.user_agent.clone()),
            odm_version: Some(VERSION.to_string()),
            checksum: Some(cf_opts.checksum.clone()),
            chunk_hashes: cf_hashes,
            profile: Some(cf_opts.profile.clone()),
            split_at: Some(self.split_at.load(Ordering::SeqCst)),
            region2_chunk_size: Some(region2),
        };
        if let Err(e) = storage::save_control(&op, &cf) {
            if !self.persist_warned.swap(true, Ordering::SeqCst) {
                // Best-effort by contract — a warn once per task.
                self.logf(
                    "warn",
                    format!("could not persist resume state {}: {e}", storage::control_path(&op)),
                );
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rfc3339_timestamp_is_go_parseable() {
        let s = rfc3339_now();
        assert_eq!(s.len(), 20, "{s}");
        assert!(s.ends_with('Z') && s.as_bytes()[10] == b'T', "{s}");
        assert_eq!(civil_from_days(0), (1970, 1, 1));
        assert_eq!(civil_from_days(20_454), (2026, 1, 1));
    }
}
