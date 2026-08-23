// Package scheduler computes connection allocation across download tasks and
// runs the parallel-slot scheduler that drives them.
//
// The Balancer (this file) is a pure function implementing the Connection
// Balancer spec from the product spec. It takes the connection budget, the set of
// URLs (with whether each supports HTTP range requests), the optional split-file
// value and the max-connections ceiling, and returns the per-file connection
// allocation plus the set of files that run in parallel vs. get queued. It
// performs no I/O.
package scheduler

import (
	"fmt"
)

// DefaultMaxConnections is the documented default for --max-connections.
const DefaultMaxConnections = 32

// FileInput describes one URL to be scheduled. SupportsRange is the result of
// the range-support probe and drives allocation-time reallocation:
// a file that cannot be ranged gets exactly one connection, and the freed
// budget is redistributed to the remaining files in the same scheduling pass.
type FileInput struct {
	URL           string
	SupportsRange bool
}

// Allocation is the Balancer's decision for a single file: how many parallel
// connections it gets, and whether it runs immediately or waits in the queue.
type Allocation struct {
	URL           string
	Connections   int
	SupportsRange bool
}

// Plan is the complete output of the Balancer: the files that start in
// parallel with their connection counts, the files that are queued, and any
// validation warning/error text.
type Plan struct {
	Parallel []Allocation // running files, order matches distribution order
	Queued   []Allocation // waiting files
	Warning  string       // non-fatal warning text (e.g. C above ceiling), "" if none
}

// Compute is the pure Connection Balancer. See the design notes.
//
// Inputs:
//   - C  total connection budget (≥1 required)
//   - files the URLs, each flagged SupportsRange from the probe
//   - SF split-file per file (only meaningful when len(files) > 1); 0 = unset
//   - maxConnections ceiling (defaults to DefaultMaxConnections when 0)
//
// Mode selection:
//   - N==1            → Mode A: whole budget to the one file
//   - N>1, SF unset   → Mode B: 1 connection/file, C controls parallel files
//   - N>1, SF set     → Mode C: SF connections/file, parallel_files derived
//
// Mode C also distributes the remainder one connection at a time to the first
// parallel files, and allocation-time reallocation redistributes budget
// freed by non-range files — applied per scheduling pass in list order.
func Compute(C int, files []FileInput, SF int, maxConnections int) (*Plan, error) {
	if maxConnections <= 0 {
		maxConnections = DefaultMaxConnections
	}
	N := len(files)
	if N == 0 {
		return nil, fmt.Errorf("no URLs provided")
	}
	if C < 1 {
		return nil, fmt.Errorf("connection budget (-c) must be at least 1")
	}

	plan := &Plan{}

	// Warning when the user explicitly raised the budget above the ceiling.
	// Still proceed — the user opted in.
	if C > maxConnections {
		plan.Warning = fmt.Sprintf(
			"connections above %d may get throttled/blocked by some servers", maxConnections)
	}

	// SF validation: only meaningful in batch mode.
	if N == 1 && SF > 0 {
		// Mode A already claims the entire budget; -sf is ignored.
		// We surface this as a non-fatal warning rather than failing.
		if plan.Warning != "" {
			plan.Warning += "; "
		}
		plan.Warning += fmt.Sprintf(
			"-sf %d ignored in single-file mode (entire budget goes to the one file)", SF)
		SF = 0
	}
	if SF > C {
		return nil, fmt.Errorf("split-file (-sf) cannot be greater than the total connection budget (-c)")
	}

	switch {
	case N == 1:
		plan.Parallel = modeA(C, files, maxConnections)
	case SF == 0:
		plan.Parallel, plan.Queued = modeB(C, files, maxConnections)
	default:
		plan.Parallel, plan.Queued = modeC(C, files, SF, maxConnections)
	}

	return plan, nil
}

// modeA — Single File: entire budget to the one file, capped by the
// ceiling. Range support is decided by the probe; if not supported the file
// still gets 1 (single-stream fallback,), and that decision is reflected
// here so callers/renders stay consistent.
func modeA(C int, files []FileInput, max int) []Allocation {
	f := files[0]
	conns := min(C, max)
	if !f.SupportsRange {
		conns = 1 // single-stream fallback
	}
	return []Allocation{{
		URL: f.URL, Connections: conns,
		SupportsRange: f.SupportsRange,
	}}
}

// modeB — Batch without -sf: each file gets 1 connection; C controls how
// many files run in parallel. parallel_files = min(C, N, max). Files beyond that
// go into the queue.
func modeB(C int, files []FileInput, max int) (parallel, queued []Allocation) {
	N := len(files)
	slot := min(C, min(N, max))
	for i, f := range files {
		a := Allocation{
			URL: f.URL, Connections: 1,
			SupportsRange: f.SupportsRange,
		}
		if i < slot {
			parallel = append(parallel, a)
		} else {
			queued = append(queued, a)
		}
	}
	return parallel, queued
}

// modeC — Batch with -sf: each file gets SF connections, parallel_files
// derived as floor(C/SF), remainder distributed one each to the first files.
// Then allocation-time reallocation: any non-range file among the
// parallel set is capped to 1, and the freed budget is redistributed one at a
// time to the other parallel files (mirroring the remainder distribution).
func modeC(C int, files []FileInput, SF int, maxConns int) (parallel, queued []Allocation) {
	N := len(files)
	parallelFiles := max(1, min(C/SF, min(N, maxConns))) // floor(C/SF), clamped to [1, min(N, max)]

	conns := make([]int, parallelFiles)
	for i := range conns {
		conns[i] = SF
	}
	used := parallelFiles * SF

	// Distribute the remainder one connection at a time, round-robin,
	// starting from the first parallel file. Round-robin (rather than a single
	// pass) keeps the budget fully used when remainder > parallel_files, and
	// "first files get more" matches the one-at-a-time-from-the-front
	// intent. At this stage no reallocation has happened, so all parallel
	// files are eligible.
	nonRange := make([]bool, parallelFiles)
	for i := range conns {
		nonRange[i] = !files[i].SupportsRange
	}
	distribute(conns, C-used, nonRange)

	// Allocation-time reallocation for non-range files. A non-range
	// parallel file is capped to exactly 1 (single-stream fallback,);
	// the budget it frees is redistributed one at a time, round-robin, to the
	// *other* parallel files (the same shape as the remainder distribution),
	// so the C budget is used to the fullest without giving extras back to a
	// single-stream file.
	freed := 0
	for i := range conns {
		if nonRange[i] && conns[i] > 1 {
			freed += conns[i] - 1
			conns[i] = 1
		}
	}
	if freed > 0 {
		distribute(conns, freed, nonRange)
	}

	for i := range N {
		a := Allocation{
			URL: files[i].URL, Connections: 1,
			SupportsRange: files[i].SupportsRange,
		}
		if i < parallelFiles {
			a.Connections = conns[i]
			parallel = append(parallel, a)
		} else {
			// Queued files inherit SF when they're admitted (Mode C per and
			// the Scheduler's contract "each queued task inherits the same
			// per-file connection budget (Mode C: SF)"). A queued single-stream
			// file still caps at 1 (single-stream fallback,).
			a.Connections = SF
			if !files[i].SupportsRange {
				a.Connections = 1
			}
			queued = append(queued, a)
		}
	}
	return parallel, queued
}

// distribute adds `budget` connections to `conns`, one at a time, round-robin
// over eligible slots (those whose `skip` entry is false), starting from slot
// 0 on each call. It mirrors the remainder rule ("one at a time to the
// first currently-running parallel files") and is reused by the
// allocation-time reallocation. Slots marked skip are never topped up — this is
// how non-range (single-stream) files are kept at their cap. `skip` may be nil
// (all eligible). If no slot is eligible the budget is simply left unused.
func distribute(conns []int, budget int, skip []bool) {
	if budget <= 0 || len(conns) == 0 {
		return
	}
	if skip == nil {
		skip = make([]bool, len(conns))
	}
	// Guard against an all-skipped set so we don't spin forever.
	any := false
	for _, s := range skip {
		if !s {
			any = true
			break
		}
	}
	if !any {
		return
	}
	for budget > 0 {
		advanced := false
		for i := range conns {
			if skip[i] {
				continue
			}
			conns[i]++
			budget--
			advanced = true
			if budget == 0 {
				break
			}
		}
		if !advanced {
			break
		}
	}
}
