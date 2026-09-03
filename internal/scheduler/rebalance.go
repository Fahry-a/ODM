package scheduler

import (
	"context"
	"sort"

	"odm/internal/download"
)

// rebalanceInput describes one live task for the runtime connection allocator.
// Non-range tasks are fixed at one connection; range-capable tasks share the
// remaining budget.
type rebalanceInput struct {
	id           download.TaskID
	rangeCapable bool
	task         *download.Task
}

// rebalanceTargets calculates deterministic connection targets without doing
// any I/O. Every live task needs one connection; all remaining budget is
// distributed evenly across range-capable tasks. If no task can use Range,
// the surplus remains intentionally unused.
func rebalanceTargets(inputs []rebalanceInput, budget int) []int {
	targets := make([]int, len(inputs))
	if len(inputs) == 0 || budget < len(inputs) {
		return targets
	}

	rangeIndexes := make([]int, 0, len(inputs))
	for i, in := range inputs {
		targets[i] = 1
		if in.rangeCapable {
			rangeIndexes = append(rangeIndexes, i)
		}
	}
	remaining := budget - len(inputs)
	if remaining <= 0 || len(rangeIndexes) == 0 {
		return targets
	}

	base := remaining / len(rangeIndexes)
	extra := remaining % len(rangeIndexes)
	for i, idx := range rangeIndexes {
		targets[idx] += base
		if i < extra {
			targets[idx]++
		}
	}
	return targets
}

// rebalanceLive moves the Mode C connection budget across currently-live
// tasks. It is called by the scheduler loop after a completion/admission event,
// so the operation is serialized with other scheduler decisions.
func (s *Scheduler) rebalanceLive(ctx context.Context) {
	s.mu.Lock()
	if !s.plan.RebalanceConnections || s.plan.ConnectionBudget < 1 || len(s.live) == 0 {
		s.mu.Unlock()
		return
	}

	inputs := make([]rebalanceInput, 0, len(s.live))
	for _, st := range s.live {
		view := st.task.Snapshot()
		inputs = append(inputs, rebalanceInput{
			id:           view.ID,
			rangeCapable: !view.SingleStream,
			task:         st.task,
		})
	}
	s.mu.Unlock()

	sort.Slice(inputs, func(i, j int) bool { return inputs[i].id < inputs[j].id })
	targets := rebalanceTargets(inputs, s.plan.ConnectionBudget)
	for i, in := range inputs {
		if targets[i] < 1 {
			continue
		}
		if !in.task.AdjustConns(targets[i], ctx, nil) {
			// A task admitted immediately before this rebalance may still be in
			// Start() and therefore reject AdjustConns. SetConns is safe before
			// Start and Start preserves a pre-raised connTarget; it is also safe
			// as a no-op for a task that just finished concurrently.
			view := in.task.Snapshot()
			if view.State != download.StateCompleted && view.State != download.StateError {
				in.task.SetConns(targets[i])
		}
		}

		s.mu.Lock()
		if live, ok := s.live[in.id]; ok && live.task == in.task {
			live.conns = targets[i]
		}
		s.mu.Unlock()
	}
}
