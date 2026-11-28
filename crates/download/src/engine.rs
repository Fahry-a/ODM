//! Rust port of ODM's `internal/download/engine.go`: split arithmetic for
//! the aria2c profile + the Engine wrapper over queue + client.

use crate::chunkqueue::{Chunk, StaticQueue, WorkQueue};
use std::collections::HashSet;
use std::sync::Arc;
use transport::Client;

/// `Engine` couples a workQueue with the transport client and byte-region
/// base it belongs to (both profile: region2 uses h2 client; base = offset).
#[derive(Clone)]
pub struct Engine {
    pub q: Arc<dyn WorkQueue>,
    pub client: Arc<Client>,
    pub base: i64,
}

impl Engine {
    pub fn new(q: Arc<dyn WorkQueue>, client: Arc<Client>, base: i64) -> Engine {
        Engine { q, client, base }
    }
    pub fn next(&self) -> Option<Chunk> {
        self.q.next()
    }
    pub fn requeue(&self, c: Chunk, max: i32) -> bool {
        self.q.requeue(c, max)
    }
    pub fn mark_done(&self, c: Chunk) {
        self.q.mark_done(c);
    }
    pub fn completed_offsets(&self) -> Vec<i64> {
        self.q.completed_offsets()
    }
    pub fn completed_spans(&self, chunk_size: i64, total_size: i64, n: usize) -> Vec<Chunk> {
        self.q.completed_spans(chunk_size, total_size, n)
    }
    pub fn reset_completed_offsets(&self, done: &HashSet<i64>, total_size: i64) -> (i64, bool) {
        self.q.reset_completed_offsets(done, total_size)
    }
    pub fn client(&self) -> &Arc<Client> {
        &self.client
    }
    pub fn abs_start(&self, rel: i64) -> i64 {
        self.base + rel
    }
}

/// `aria_split` computes the effective split count and segment size for a
/// file of totalSize given --split and --min-split-size (aria2c semantics).
pub fn aria_split(total_size: i64, split: i64, min_split: i64) -> (i64, i64) {
    if total_size <= 0 || split < 1 {
        return (1, total_size);
    }
    let min_split = if min_split < 1 { 1 } else { min_split };
    let mut n = split;
    let max_n = total_size / (2 * min_split);
    if max_n < n && max_n >= 1 {
        n = max_n;
    }
    (n, (total_size + n - 1) / n)
}

pub fn new_static_queue(total_size: i64, n: i64) -> StaticQueue {
    StaticQueue::new(total_size, n)
}

pub fn new_chunk_queue(total_size: i64, chunk_size: i64) -> crate::chunkqueue::ChunkQueue {
    crate::chunkqueue::ChunkQueue::new(total_size, chunk_size)
}
