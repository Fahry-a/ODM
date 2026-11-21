//! Rust port of ODM's `internal/download/chunkqueue.go` + `engine.go`.
//!
//! `ChunkQueue` is the shared, per-task work list (odm profile): workers pull
//! the next available chunk as soon as they finish their previous one,
//! instead of each owning a fixed byte-range. `StaticQueue` is the aria2c
//! static split: one segment per worker, no work-stealing.

use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::Mutex;

/// `Chunk` is one slice of work for a worker: download [Start, End]
/// inclusive and write it to the destination at offset Start.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Chunk {
    pub index: i64,
    pub start: i64,
    /// inclusive; `end < 0` ⇒ to EOF/unknown length.
    pub end: i64,
}

/// `WorkQueue` is the common surface the worker loop needs from a chunk
/// source.
pub trait WorkQueue: Send + Sync {
    fn next(&self) -> Option<Chunk>;
    fn requeue(&self, c: Chunk, max_worker_attempts: i32) -> bool;
    fn mark_done(&self, c: Chunk);
    fn completed_offsets(&self) -> Vec<i64>;
    fn completed_spans(&self, chunk_size: i64, total_size: i64, n: usize) -> Vec<Chunk>;
    fn reset_completed_offsets(&self, done: &HashSet<i64>, total_size: i64) -> (i64, bool);
    fn is_static(&self) -> bool {
        false
    }
}

pub const DEFAULT_CHUNK_SIZE: i64 = 4 * 1024 * 1024;

fn chunk_bytes(c: Chunk, total_size: i64) -> i64 {
    if c.end < 0 || total_size < 0 {
        if total_size > 0 {
            total_size
        } else {
            0
        }
    } else {
        c.end - c.start + 1
    }
}

fn span(off: i64, chunk_size: i64, total_size: i64) -> Chunk {
    let mut end = off + chunk_size - 1;
    if end >= total_size {
        end = total_size - 1;
    }
    Chunk { start: off, end, index: -1 }
}

#[derive(Default)]
struct ChunkQueueInner {
    chunks: VecDeque<Chunk>,
    completed: HashSet<i64>,
    failed: HashMap<i64, i32>,
}

/// `ChunkQueue` — the shared, per-task work list (fixed chunks, work-stealing).
pub struct ChunkQueue {
    mu: Mutex<ChunkQueueInner>,
}

impl ChunkQueue {
    /// `new` builds the chunk list for a file of totalSize using chunkSize
    /// bytes per chunk. If totalSize < 0 the queue has a single whole-file
    /// chunk (Start=0 End=-1) — the single-stream degeneration.
    pub fn new(total_size: i64, chunk_size: i64) -> ChunkQueue {
        let q = ChunkQueue { mu: Mutex::new(ChunkQueueInner::default()) };
        if total_size < 0 {
            q.mu.lock().unwrap().chunks.push_back(Chunk { index: 0, start: 0, end: -1 });
            return q;
        }
        let chunk_size = if chunk_size < 1 { DEFAULT_CHUNK_SIZE } else { chunk_size };
        if total_size == 0 {
            return q;
        }
        let mut chunks = VecDeque::new();
        let mut idx = 0i64;
        let mut start = 0i64;
        while start < total_size {
            let mut end = start + chunk_size - 1;
            if end >= total_size {
                end = total_size - 1;
            }
            chunks.push_back(Chunk { index: idx, start, end });
            start = end + 1;
            idx += 1;
        }
        *q.mu.lock().unwrap() = ChunkQueueInner { chunks, ..Default::default() };
        q
    }
}

impl WorkQueue for ChunkQueue {
    fn next(&self) -> Option<Chunk> {
        self.mu.lock().unwrap().chunks.pop_front()
    }
    fn requeue(&self, c: Chunk, max_worker_attempts: i32) -> bool {
        let mut inner = self.mu.lock().unwrap();
        let count = inner.failed.entry(c.index).or_insert(0);
        *count += 1;
        if *count > max_worker_attempts {
            return false;
        }
        inner.chunks.push_back(c);
        true
    }
    fn mark_done(&self, c: Chunk) {
        self.mu.lock().unwrap().completed.insert(c.start);
    }
    fn completed_offsets(&self) -> Vec<i64> {
        let inner = self.mu.lock().unwrap();
        let mut out: Vec<i64> = inner.completed.iter().copied().collect();
        out.sort_unstable();
        out
    }
    fn completed_spans(&self, chunk_size: i64, total_size: i64, n: usize) -> Vec<Chunk> {
        let inner = self.mu.lock().unwrap();
        let mut starts: Vec<i64> = inner.completed.iter().copied().collect();
        starts.sort_unstable();
        let mut n = n;
        if n == 0 || starts.len() <= n {
            n = starts.len();
        }
        let mut out = Vec::new();
        for off in starts.iter().take(n) {
            out.push(span(*off, chunk_size, total_size));
        }
        out
    }
    fn reset_completed_offsets(&self, done: &HashSet<i64>, total_size: i64) -> (i64, bool) {
        let mut inner = self.mu.lock().unwrap();
        inner.failed = HashMap::new();
        if done.len() > inner.chunks.len() {
            return (0, false);
        }
        inner.completed = done.clone();
        let mut already = 0i64;
        let mut kept: VecDeque<Chunk> = VecDeque::new();
        for c in inner.chunks.iter() {
            if done.contains(&c.start) {
                already += chunk_bytes(*c, total_size);
            } else {
                kept.push_back(*c);
            }
        }
        inner.chunks = kept;
        (already, true)
    }
}

#[derive(Default)]
struct StaticQueueInner {
    chunks: Vec<Chunk>,
    next: usize,
    done: HashSet<i64>,
}

/// `StaticQueue` — a fixed-list work queue with no work-stealing: exactly n
/// segments covering [0, totalSize) contiguously (last one possibly short).
pub struct StaticQueue {
    mu: Mutex<StaticQueueInner>,
}

impl StaticQueue {
    pub fn new(total_size: i64, n: i64) -> StaticQueue {
        let n = if n < 1 { 1 } else { n };
        let q = StaticQueue { mu: Mutex::new(StaticQueueInner::default()) };
        if total_size <= 0 {
            return q;
        }
        let seg = (total_size + n - 1) / n;
        let mut chunks = Vec::new();
        let mut start = 0i64;
        while start < total_size {
            let mut end = start + seg - 1;
            if end >= total_size {
                end = total_size - 1;
            }
            chunks.push(Chunk { index: chunks.len() as i64, start, end });
            start = end + 1;
        }
        *q.mu.lock().unwrap() = StaticQueueInner { chunks, ..Default::default() };
        q
    }
}

impl WorkQueue for StaticQueue {
    fn next(&self) -> Option<Chunk> {
        let mut inner = self.mu.lock().unwrap();
        while inner.next < inner.chunks.len() {
            let c = inner.chunks[inner.next];
            inner.next += 1;
            if !inner.done.contains(&c.start) {
                return Some(c);
            }
        }
        None
    }
    fn requeue(&self, _c: Chunk, _max: i32) -> bool {
        false // aria2c model: per-worker retry budget inside the worker
    }
    fn mark_done(&self, c: Chunk) {
        self.mu.lock().unwrap().done.insert(c.start);
    }
    fn completed_offsets(&self) -> Vec<i64> {
        let inner = self.mu.lock().unwrap();
        let mut out: Vec<i64> = inner.done.iter().copied().collect();
        out.sort_unstable();
        out
    }
    fn completed_spans(&self, chunk_size: i64, total_size: i64, n: usize) -> Vec<Chunk> {
        let inner = self.mu.lock().unwrap();
        let mut starts: Vec<i64> = inner.done.iter().copied().collect();
        starts.sort_unstable();
        let mut n = n;
        if n == 0 || starts.len() <= n {
            n = starts.len();
        }
        let mut out = Vec::new();
        for off in starts.iter().take(n) {
            out.push(span(*off, chunk_size, total_size));
        }
        out
    }
    fn reset_completed_offsets(&self, done: &HashSet<i64>, _total_size: i64) -> (i64, bool) {
        let mut inner = self.mu.lock().unwrap();
        let mut already = 0i64;
        let mut hits: Vec<i64> = Vec::new();
        for c in inner.chunks.iter() {
            if done.contains(&c.start) {
                hits.push(c.start);
                already += c.end - c.start + 1;
            }
        }
        for s in hits {
            inner.done.insert(s);
        }
        inner.next = 0;
        while inner.next < inner.chunks.len() {
            if inner.done.contains(&inner.chunks[inner.next].start) {
                inner.next += 1;
            } else {
                break;
            }
        }
        (already, true)
    }
    fn is_static(&self) -> bool {
        true
    }
}
