//! Rust port of ODM's `internal/scheduler/rebalance.go` — runtime connection
//! allocator for live Mode C tasks. Pure function.

use std::sync::Arc;

use download::{Task, TaskID};

/// `RebalanceInput` — one live task for the runtime connection allocator.
/// Non-range tasks are fixed at one connection; range-capable tasks share
/// the remaining budget.
pub struct RebalanceInput {
    pub id: TaskID,
    pub range_capable: bool,
    pub task: Arc<Task>,
}

/// `rebalanceTargets` — deterministic connection targets without any I/O.
/// Every live task gets one connection; all remaining budget is distributed
/// evenly across range-capable tasks (extras first for the lowest indexes).
/// If no task can use Range, the surplus is left intentionally unused.
pub fn rebalance_targets(inputs: &[RebalanceInput], budget: i32) -> Vec<i32> {
    if inputs.is_empty() || budget < inputs.len() as i32 {
        return vec![0; inputs.len()];
    }

    let mut range_indexes: Vec<usize> = Vec::with_capacity(inputs.len());
    for (i, inp) in inputs.iter().enumerate() {
        if inp.range_capable {
            range_indexes.push(i);
        }
    }
    let remaining = budget - inputs.len() as i32;
    if remaining <= 0 || range_indexes.is_empty() {
        return vec![1; inputs.len()];
    }

    let base = remaining / range_indexes.len() as i32;
    let extra = remaining % range_indexes.len() as i32;
    let mut targets = vec![1; inputs.len()];
    for (i, idx) in range_indexes.iter().enumerate() {
        targets[*idx] += base;
        if i < extra as usize {
            targets[*idx] += 1;
        }
    }
    targets
}

#[cfg(test)]
mod tests {
    use super::*;

    fn input(id: &str, range_capable: bool) -> RebalanceInput {
        let t = Task::new(
            "x".to_string(),
            "y",
            Default::default(),
            Arc::new(transport::new_client(transport::ClientConfig::default()).unwrap()),
            Arc::new(ratelimit::Limiter::default()),
            None,
        );
        RebalanceInput { id: id.to_string(), range_capable, task: t }
    }

    #[test]
    fn single_remaining_gets_full_budget() {
        let inputs = vec![input("odm-1", true), input("odm-2", true)];
        let got = rebalance_targets(&inputs, 25);
        assert_eq!(got, vec![13, 12]);

        let remaining = vec![input("odm-2", true)];
        let got = rebalance_targets(&remaining, 25);
        assert_eq!(got, vec![25]);
    }

    #[test]
    fn non_range_surplus_goes_to_range() {
        let inputs = vec![input("odm-1", false), input("odm-2", true)];
        let got = rebalance_targets(&inputs, 25);
        assert_eq!(got, vec![1, 24]);
    }

    #[test]
    fn evenly_distributes_range_budget() {
        let inputs = vec![input("odm-1", true), input("odm-2", true), input("odm-3", true)];
        let got = rebalance_targets(&inputs, 10);
        assert_eq!(got, vec![4, 3, 3]);
    }

    #[test]
    fn all_non_range_keeps_single_stream() {
        let inputs = vec![input("odm-1", false), input("odm-2", false)];
        let got = rebalance_targets(&inputs, 25);
        assert_eq!(got, vec![1, 1]);
    }

    #[test]
    fn zero_and_insufficient_budget() {
        let inputs = vec![input("odm-1", true), input("odm-2", true)];
        assert!(rebalance_targets(&[], 25).is_empty());
        let got = rebalance_targets(&inputs, 1);
        assert_eq!(got, vec![0, 0]);
    }
}
