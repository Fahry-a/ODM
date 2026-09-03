package scheduler

import "sync/atomic"

// SucceededCount and FailedCount expose live scheduler tallies used by tests
// and daemon lifecycle checks.
func (s *Scheduler) SucceededCount() int { return int(atomic.LoadInt32(&s.succeeded)) }
func (s *Scheduler) FailedCount() int    { return int(atomic.LoadInt32(&s.failed)) }
