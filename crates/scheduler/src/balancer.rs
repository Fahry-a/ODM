//! Rust port of ODM's `internal/scheduler/balancer.go` — the pure Connection
//! Balancer: connection-budget allocation across download files. Performs no
//! I/O.

use std::cmp::{max, min};

/// `DefaultMaxConnections` — the documented default for --max-connections.
pub const DEFAULT_MAX_CONNECTIONS: i32 = 32;

/// `FileInput` — one URL to be scheduled. SupportsRange is the result of the
/// range-support probe and drives allocation-time reallocation.
#[derive(Debug, Clone)]
pub struct FileInput {
    pub url: String,
    pub supports_range: bool,
}

/// `Allocation` — the Balancer's decision for a single file: how many
/// parallel connections it gets, and whether it runs immediately or waits in
/// the queue.
#[derive(Debug, Clone)]
pub struct Allocation {
    pub url: String,
    pub connections: i32,
    pub supports_range: bool,
}

/// `Plan` — the complete output of the Balancer.
#[derive(Debug, Clone, Default)]
pub struct Plan {
    /// Running files, order matches distribution order.
    pub parallel: Vec<Allocation>,
    /// Waiting files.
    pub queued: Vec<Allocation>,
    /// Non-fatal warning text ("", if none).
    pub warning: String,
    /// Original -c budget (runtime balancing moves it between live tasks).
    pub connection_budget: i32,
    /// True only for Mode C (explicit split-file batch mode).
    pub rebalance_connections: bool,
}

/// `Compute` — the pure Connection Balancer. Mode selection:
///   - N==1            → Mode A: whole budget to the one file
///   - N>1, SF unset   → Mode B: 1 connection/file, C controls parallel files
///   - N>1, SF set     → Mode C: SF connections/file, parallel_files derived
///
/// Mode C also distributes the remainder one connection at a time to the first
/// parallel files, and allocation-time reallocation redistributes budget
/// freed by non-range files. The live scheduler may additionally rebalance the
/// same total budget whenever a Mode C task leaves or enters the live set.
pub fn compute(c: i32, files: &[FileInput], sf: i32, max_connections: i32) -> Result<Plan, String> {
    let max_connections = if max_connections <= 0 { DEFAULT_MAX_CONNECTIONS } else { max_connections };
    let n = files.len();
    if n == 0 {
        return Err("no URLs provided".to_string());
    }
    if c < 1 {
        return Err("connection budget (-c) must be at least 1".to_string());
    }

    let mut plan = Plan { connection_budget: c, ..Default::default() };

    // Warning when the user explicitly raised the budget above the ceiling.
    // Still proceed — the user opted in.
    if c > max_connections {
        plan.warning = format!("connections above {max_connections} may get throttled/blocked by some servers");
    }

    // SF validation: only meaningful in batch mode.
    let mut sf = sf;
    if n == 1 && sf > 0 {
        // Mode A already claims the entire budget; -sf is ignored. We surface
        // this as a non-fatal warning rather than failing.
        if !plan.warning.is_empty() {
            plan.warning += "; ";
        }
        plan.warning += &format!("-sf {sf} ignored in single-file mode (entire budget goes to the one file)");
        sf = 0;
    }
    if sf > c {
        return Err("split-file (-sf) cannot be greater than the total connection budget (-c)".to_string());
    }

    if n == 1 {
        plan.parallel = mode_a(c, files, max_connections);
    } else if sf == 0 {
        let (parallel, queued) = mode_b(c, files, max_connections);
        plan.parallel = parallel;
        plan.queued = queued;
    } else {
        plan.rebalance_connections = true;
        let (parallel, queued) = mode_c(c, files, sf, max_connections);
        plan.parallel = parallel;
        plan.queued = queued;
    }
    Ok(plan)
}

/// `modeA` — Single File: entire budget to the one file, capped by the
/// ceiling. Range support is decided by the probe; if not supported the file
/// still gets 1 (single-stream fallback).
fn mode_a(c: i32, files: &[FileInput], max: i32) -> Vec<Allocation> {
    let f = &files[0];
    let mut conns = min(c, max);
    if !f.supports_range {
        conns = 1;
    }
    vec![Allocation { url: f.url.clone(), connections: conns, supports_range: f.supports_range }]
}

/// `modeB` — Batch without -sf: each file gets 1 connection; C controls how
/// many files run in parallel (parallel_files = min(C, N, max)).
fn mode_b(c: i32, files: &[FileInput], max: i32) -> (Vec<Allocation>, Vec<Allocation>) {
    let slot = min(c, min(files.len() as i32, max));
    let mut parallel = Vec::new();
    let mut queued = Vec::new();
    for (i, f) in files.iter().enumerate() {
        let a = Allocation { url: f.url.clone(), connections: 1, supports_range: f.supports_range };
        if (i as i32) < slot {
            parallel.push(a);
        } else {
            queued.push(a);
        }
    }
    (parallel, queued)
}

/// `modeC` — Batch with -sf: each file gets SF connections, parallel derived
/// as floor(C/SF) clamped to [1, min(N, max)]; remainder distributed one at a
/// time to the first parallel files; then allocation-time reallocation caps
/// non-range files to 1 and redistributes the freed budget.
fn mode_c(c: i32, files: &[FileInput], sf: i32, max_conns: i32) -> (Vec<Allocation>, Vec<Allocation>) {
    let n = files.len() as i32;
    let parallel_files = max(1, min(c / sf, min(n, max_conns)));

    let mut conns: Vec<i32> = vec![sf; parallel_files as usize];
    let used = parallel_files * sf;
    let mut non_range = vec![false; parallel_files as usize];
    for i in 0..parallel_files as usize {
        non_range[i] = !files[i].supports_range;
    }
    distribute(&mut conns, c - used, &non_range);

    // Allocation-time reallocation: a non-range parallel file is capped to
    // exactly 1; the budget it frees goes back to the other range-capable
    // parallel files (non-range files are never topped up).
    let mut freed = 0;
    for i in 0..parallel_files as usize {
        if non_range[i] && conns[i] > 1 {
            freed += conns[i] - 1;
            conns[i] = 1;
        }
    }
    if freed > 0 {
        distribute(&mut conns, freed, &non_range);
    }

    let mut parallel = Vec::new();
    let mut queued = Vec::new();
    for (i, f) in files.iter().enumerate() {
        let mut a = Allocation { url: f.url.clone(), connections: 1, supports_range: f.supports_range };
        if i < parallel_files as usize {
            a.connections = conns[i];
            parallel.push(a);
        } else {
            // Queued files inherit SF when admitted (Mode C contract); a
            // queued single-stream file still caps at 1.
            a.connections = sf;
            if !f.supports_range {
                a.connections = 1;
            }
            queued.push(a);
        }
    }
    (parallel, queued)
}

/// `distribute` — adds budget one at a time round-robin over eligible slots
/// (skip[i]==false), starting at 0 each call. Slots marked skip are never
/// topped up — this is how non-range (single-stream) files stay at their cap.
/// `budget <= 0` or an all-skipped set is a no-op; if no slot is eligible the
/// budget is left unused.
fn distribute(conns: &mut [i32], budget: i32, skip: &[bool]) {
    if budget <= 0 || conns.is_empty() {
        return;
    }
    if skip.iter().all(|s| *s) {
        return;
    }
    let mut budget = budget;
    while budget > 0 {
        let mut advanced = false;
        for i in 0..conns.len() {
            if skip[i] {
                continue;
            }
            conns[i] += 1;
            budget -= 1;
            advanced = true;
            if budget == 0 {
                break;
            }
        }
        if !advanced {
            break;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mk_range(n: usize) -> Vec<FileInput> {
        (0..n).map(|_| FileInput { url: "u".into(), supports_range: true }).collect()
    }

    fn total_conns(p: &Plan) -> i32 {
        p.parallel.iter().map(|a| a.connections).sum()
    }

    #[test]
    fn mode_a_all_budget_to_single_file() {
        let p = compute(16, &mk_range(1), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 1);
        assert_eq!(p.parallel[0].connections, 16);
        assert!(p.queued.is_empty());
    }

    #[test]
    fn mode_a_capped_by_max() {
        let p = compute(100, &mk_range(1), 0, 32).unwrap();
        assert_eq!(p.parallel[0].connections, 32);
        assert!(!p.warning.is_empty(), "want warning for C>max");
    }

    #[test]
    fn mode_a_no_range_support_single_stream() {
        let files = vec![FileInput { url: "u".into(), supports_range: false }];
        let p = compute(16, &files, 0, 32).unwrap();
        assert_eq!(p.parallel[0].connections, 1);
    }

    #[test]
    fn mode_b_files_in_parallel_queued() {
        let p = compute(16, &mk_range(16), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 16);
        assert_eq!(p.queued.len(), 0);
        assert!(p.parallel.iter().all(|a| a.connections == 1));
    }

    #[test]
    fn mode_b_more_urls_than_budget() {
        let p = compute(8, &mk_range(16), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 8);
        assert_eq!(p.queued.len(), 8);
    }

    #[test]
    fn mode_b_capped_by_max_connections() {
        let p = compute(64, &mk_range(64), 0, 32).unwrap();
        assert_eq!(p.parallel.len(), 32);
        assert_eq!(p.queued.len(), 32);
    }

    #[test]
    fn mode_c_spec_example() {
        // spec example: -c 16 -sf 5, 10 urls. parallel_files = floor(16/5)=3,
        // used=15, remainder=1 → [6,5,5], 0 queued? No — wait: Go says 7 queued.
        let p = compute(16, &mk_range(10), 5, 32).unwrap();
        assert_eq!(p.parallel.len(), 3);
        assert_eq!(p.queued.len(), 7);
        let got: Vec<i32> = p.parallel.iter().map(|a| a.connections).collect();
        assert_eq!(got, vec![6, 5, 5]);
        assert_eq!(total_conns(&p), 16);
    }

    #[test]
    fn mode_c_remainder_distribution() {
        // -c 10 -sf 3: floor(10/3)=3, used=9, remainder=1 → conns=[4,3,3].
        let p = compute(10, &mk_range(6), 3, 32).unwrap();
        let got: Vec<i32> = p.parallel.iter().map(|a| a.connections).collect();
        assert_eq!(got, vec![4, 3, 3]);
    }

    #[test]
    fn mode_c_sf_exceeds_parallel_uses_full_n() {
        // -c 16 -sf 5, 3 urls: floor(16/5)=3 but N=3 → 3 parallel, used=15,
        // remainder=1 → [6,5,5], 0 queued, total 16.
        let p = compute(16, &mk_range(3), 5, 32).unwrap();
        assert_eq!(p.parallel.len(), 3);
        assert_eq!(p.queued.len(), 0);
        assert_eq!(total_conns(&p), 16);
    }

    #[test]
    fn mode_c_queued_inherits_sf() {
        // -c 10 -sf 3, 8 urls: 3 parallel (4/3/3), 5 queued — each queued
        // file inherits SF=3, not the Mode-B default of 1.
        let p = compute(10, &mk_range(8), 3, 32).unwrap();
        assert_eq!(p.parallel.len(), 3);
        assert_eq!(p.queued.len(), 5);
        assert!(p.queued.iter().all(|a| a.connections == 3));
    }

    #[test]
    fn mode_c_queued_single_stream_caps_at_1() {
        let files = vec![
            FileInput { url: "a".into(), supports_range: true },
            FileInput { url: "b".into(), supports_range: true },
            FileInput { url: "c".into(), supports_range: false },
        ];
        let p = compute(6, &files, 2, 32).unwrap();
        for a in &p.queued {
            if !a.supports_range {
                assert_eq!(a.connections, 1);
            }
        }
    }

    #[test]
    fn validation_c_below_one() {
        assert!(compute(0, &mk_range(1), 0, 32).is_err());
    }

    #[test]
    fn validation_sf_greater_than_c() {
        let err = compute(4, &mk_range(8), 5, 32).unwrap_err();
        assert!(err.contains("cannot be greater"));
    }

    #[test]
    fn validation_sf_ignored_in_single_file() {
        let p = compute(16, &mk_range(1), 4, 32).unwrap();
        assert_eq!(p.parallel[0].connections, 16);
        assert!(p.warning.contains("ignored in single-file mode"));
    }

    #[test]
    fn allocation_time_reallocation() {
        // A non-range parallel file is capped to 1; its freed budget is
        // redistributed to the other parallel files (total stays 16).
        let mut files = mk_range(10);
        files[0].supports_range = false;
        let p = compute(16, &files, 5, 32).unwrap();
        assert_eq!(p.parallel[0].connections, 1);
        assert_eq!(total_conns(&p), 16);
    }

    #[test]
    fn allocation_time_reallocation_never_adds_to_non_range() {
        // All parallel files non-range: each capped to 1; nothing to
        // redistribute to; surplus simply unused. Total = 3.
        let files: Vec<FileInput> = (0..10).map(|_| FileInput { url: "u".into(), supports_range: false }).collect();
        let p = compute(16, &files, 5, 32).unwrap();
        assert!(p.parallel.iter().all(|a| a.connections == 1));
        assert_eq!(total_conns(&p), 3);
    }
}
