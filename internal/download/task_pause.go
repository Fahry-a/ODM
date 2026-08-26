package download

import (
	"context"

	"odm/internal/ratelimit"
)

// unlimited. Used by RPC changeOption with "max-download-limit-per-task". Safe
// for concurrent use: creates a new limiter atomically (readers snapshot
// t.taskLim when wrapping the body, so an in-flight read finishes with the old
// value; subsequent reads pick up the new one).
func (t *Task) SetTaskRate(spec string) bool {
	l, err := ratelimit.New(spec)
	if err != nil {
		return false
	}
	t.taskLim.Store(l)
	return true
}

// Pause / Unpause are RPC-facing hooks.
//
// The pause mechanism is a broadcast-close wake-up, NOT a one-shot signal:
//   - Pause sets the `paused` flag; workers rendezvous in pauseGate (below),
//     which blocks on the current pauseC channel.
//   - Unpause clears the flag, CLOSES pauseC, and installs a fresh channel.
//     Close is a broadcast: every worker blocked in pauseGate wakes, re-checks
//     the flag under t.mu, and proceeds. A single send on a buffered(1) channel
//     would instead wake only ONE of N blocked workers and leave the rest
//     blocked forever (Start's workerWg.Wait would never return) — that is the
//     bug this design replaces. The fresh channel ensures the next Pause has
//     an open channel for workers to block on again (a closed channel would
//     let them spin instead of sleeping).
//
// The gate reads the channel atomically with the flag (under t.mu), so a
// worker can never block on a channel that a concurrent Unpause has already
// closed-and-replaced. See pauseGate.
// Pause / Unpause are RPC-facing hooks.
//
// The pause mechanism is a broadcast-close wake-up, NOT a one-shot signal:
//   - Pause sets the `paused` flag; workers rendezvous in pauseGate (below),
//     which blocks on the current pauseC channel.
//   - Unpause clears the flag, CLOSES pauseC, and installs a fresh channel.
//     Close is a broadcast: every worker blocked in pauseGate wakes, re-checks
//     the flag under t.mu, and proceeds. A single send on a buffered(1) channel
//     would instead wake only ONE of N blocked workers and leave the rest
//     blocked forever (Start's workerWg.Wait would never return) — that is the
//     bug this design replaces. The fresh channel ensures the next Pause has
//     an open channel for workers to block on again (a closed channel would
//     let them spin instead of sleeping).
//
// The gate reads the channel atomically with the flag (under t.mu), so a
// worker can never block on a channel that a concurrent Unpause has already
// closed-and-replaced. See pauseGate.
func (t *Task) Pause() {
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	t.setState(StatePaused)
}
func (t *Task) Unpause() {
	t.mu.Lock()
	if t.paused {
		t.paused = false
		close(t.pauseC)                // broadcast: wake ALL workers in pauseGate
		t.pauseC = make(chan struct{}) // fresh channel for the next pause cycle
	}
	t.mu.Unlock()
	t.setState(StateActive)
}

// pauseGate blocks the calling worker while the task is paused, returning when
// the task is unpaused (or ctx is cancelled). It is the worker-loop pause
// gate: a worker that reaches it while paused sleeps on pauseC instead of
// burning CPU or draining the queue.
//
// Correctness: the channel is read under t.mu — atomically with the `paused`
// flag — so the wait target is always the channel current for this pause
// cycle. Unpause closes that channel (waking every waiter) before replacing
// it, so no worker can be left sleeping on a stale channel after unpausing.
// The `for t.paused` re-check handles a pause that races the wake-up: a worker
// woken by a close re-locks, sees paused==true (a new Pause won the race),
// and blocks on the freshly-installed channel.
// pauseGate blocks the calling worker while the task is paused, returning when
// the task is unpaused (or ctx is cancelled). It is the worker-loop pause
// gate: a worker that reaches it while paused sleeps on pauseC instead of
// burning CPU or draining the queue.
//
// Correctness: the channel is read under t.mu — atomically with the `paused`
// flag — so the wait target is always the channel current for this pause
// cycle. Unpause closes that channel (waking every waiter) before replacing
// it, so no worker can be left sleeping on a stale channel after unpausing.
// The `for t.paused` re-check handles a pause that races the wake-up: a worker
// woken by a close re-locks, sees paused==true (a new Pause won the race),
// and blocks on the freshly-installed channel.
func (t *Task) pauseGate(ctx context.Context) {
	t.mu.Lock()
	for t.paused {
		ch := t.pauseC
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
		t.mu.Lock()
	}
	t.mu.Unlock()
}
func (t *Task) Cancel() {
	t.cancelled.Store(true)
	if t.cancel != nil {
		t.cancel()
	}
}

// CompletedOffsets returns the completed chunk offsets; it takes the queue's
// own lock. Implements the workQueue interface shared with StaticQueue.
