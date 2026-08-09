// Package ui renders ODM's pacman/CachyOS (ILoveCandy) progress bar (PRD §8),
// runs the live TTY redraw loop over a snapshot feed, and shows the §9
// confirmation prompt. It is the only screen-printing layer of the engine —
// the download + scheduler packages send ProgressView snapshots; ui owns every
// byte that reaches stdout.
//
// This file owns the stateful Renderer: a per-task-ID snapshot cache plus the
// live / non-TTY redraw loop. The cache is what fixes the project's headline
// progress bug ("Total: 0/0", completed lines vanishing mid-stream); see
// cache notes on Frame and Frame's vanish handling below.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"odm/internal/download"
)

// ANSI control sequences we use.
const (
	ansiClearLine  = "\x1b[2K"
	ansiCursorUp   = "\x1b[A"
	ansiCursorHide = "\x1b[?25l"
	ansiCursorShow = "\x1b[?25h"
)

// shouldColor reports whether ANSI colours should be emitted: only on a TTY and
// when NO_COLOR isn't set (PRD §8).
func shouldColor(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if f, ok := w.(*os.File); ok {
		fi, err := f.Stat()
		if err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			return true
		}
	}
	return false
}

// IsTTY reports whether w is a character device (a real terminal).
func IsTTY(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		fi, err := f.Stat()
		return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
	}
	return false
}

// cursor holds the live + queued snapshot slices Frame last received, and the
// per-task-ID cache is layered on top so a task that vanishes from the live set
// (the scheduler moves it to the stopped set on completion — which never reaches
// ProgressCB) is retained at its terminal state instead of dropping to zero.
type cursor struct {
	cache map[download.TaskID]download.ProgressView
	live  []download.ProgressView
	queue []download.ProgressView
}

// orderCache returns the cached snapshots in a stable order: live first (in the
// order of the latest live slice), then any cache-only ids (retired tasks) in
// ascending id order, then queued. Stable ordering keeps the redraw from
// shuffling lines between frames.
func (c *cursor) ordered() []download.ProgressView {
	out := make([]download.ProgressView, 0, len(c.cache))
	seen := make(map[download.TaskID]struct{}, len(c.cache))
	for _, v := range c.live {
		out = append(out, v)
		seen[v.ID] = struct{}{}
	}
	// ids present in the cache but absent from the latest live slice: the task
	// has either finished (retired out of the scheduler's live map) or its line
	// is momentarily gone. We retain it so the completed/error line stays on
	// screen at its true final state.
	var leftovers []download.ProgressView
	for id, v := range c.cache {
		if _, ok := seen[id]; ok {
			continue
		}
		leftovers = append(leftovers, v)
	}
	// deterministic: by id so the same set always renders the same way.
	slices.SortStableFunc(leftovers, func(a, b download.ProgressView) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	out = append(out, leftovers...)
	return out
}

// Renderer is the live redraw state:
//   - cur - the per-task cache + last live/queued slices (see Frame)
//   - lastLines: how many physical lines the previous frame drew, so the next
//     frame moves the cursor up that many rows to overwrite in place
//   - indeterminateTick: the bouncing frames counter for sizeless bars
//   - sizeFn - the terminal-size probe; tests swap it to pin width/height
//   - nonTTYInterval / lastNonTTYFlush / lastLogged*: the throttle bookkeeping
type Renderer struct {
	w        io.Writer
	useColor bool
	tty      bool
	quiet    bool

	// sizeFn returns the terminal size for r.w. Defaults to rendererSize;
	// tests replace it to render into a fixed-size "terminal".
	sizeFn func(io.Writer) (width, height int)

	cur cursor

	lastLines int

	// indeterminateTick advances the sizeless pacman bounce once per Frame so
	// it visibly moves between ticks (bug §3.5). Reset for nothing — a missed
	// frame just delays the bounce, never corrupts state.
	indeterminateTick int

	// Wake is a notification channel the RunLoop drains to re-render
	// immediately. main.go pings it on SIGWINCH so a terminal resize lands on
	// the next frame instead of waiting out the 100ms tick. Buffer 1 and
	// non-blocking send: coalesced, never blocks the sender.
	Wake chan struct{}

	// Non-TTY throttle (PRD §8.2): when stdout is redirected we don't want a
	// line every 100ms. We print at most one summary snapshot per
	// nonTTYInterval, and additionally whenever the aggregate state changes
	// (a task completes/pauses, or the batch's aggregate percentage crosses an
	// integer threshold) so a reader of the log still sees milestones.
	nonTTYInterval  time.Duration
	lastNonTTYFlush time.Time
	// lastLoggedPct is the aggregate percentage we last printed, so the
	// "crossed an integer threshold" rule only fires on real progress.
	lastLoggedPct int
	// lastLoggedDoneKey is a cheap fingerprint of (count completed, sum done)
	// so a state change with no byte progress doesn't re-emit a near-identical
	// line every interval.
	lastLoggedDoneKey string
}

// NewRenderer builds a Renderer writing to w. It auto-downgrades to non-TTY
// fallback when w is not a terminal or quiet is set.
func NewRenderer(w io.Writer, quiet bool) *Renderer {
	tty := !quiet && IsTTY(w)
	return &Renderer{
		w:        w,
		tty:      tty,
		useColor: tty && shouldColor(w),
		quiet:    quiet,
		sizeFn:   rendererSize,
		Wake:     make(chan struct{}, 1),
		cur: cursor{
			cache: map[download.TaskID]download.ProgressView{},
		},
		// PRD §8.2: "every 10% or every fixed time interval". One line/2s is
		// readable in a redirected log without drowning it; the state-change
		// path adds milestone lines on top.
		nonTTYInterval: 2 * time.Second,
	}
}

// Begin hides the cursor (TTY mode).
func (r *Renderer) Begin() {
	if r.tty {
		fmt.Fprint(r.w, ansiCursorHide)
	}
}

// End shows the cursor again + trailing newline so the shell prompt lands below.
func (r *Renderer) End() {
	if r.tty {
		fmt.Fprint(r.w, ansiCursorShow)
	}
	fmt.Fprintln(r.w)
}

// updateCache merges the frame's live + queued snapshots into r.cur.cache and
// refreshes the cached live/queue slices. It returns the full ordered view to
// render (live, retained-retired, queued). Two rules fix the headline bug:
//
//  1. A task present in the cache but absent from BOTH live and queued is
//     treated as retired: we promote it to its terminal state — Completed
//     (green) when bytesDone>=totalSize or the size was unknown, else we keep
//     its last state. This is the "vanished completed line must stay on screen
//     at 100%" fix (bug §3.1): the scheduler deletes a finished task from its
//     live map the instant it completes and never forwards the terminal
//     snapshot, so without retention the bar blinks to zero and the summary
//     reads Total: 0/0.
//  2. nil live+queued (the post-Run "final frame" call from main.go) is a
//     request to re-render from cache, not to clear it — Frame(nil,nil) must
//     still show the completed batch.
func (r *Renderer) updateCache(live, queued []download.ProgressView) []download.ProgressView {
	if live == nil && queued == nil {
		// Final-frame call: keep cache as-is, just re-emit. Don't touch the
		// retired-promotion path — promotion already happened in prior frames.
		return r.cur.ordered()
	}

	// Seed cache / refresh known snapshots.
	for _, v := range live {
		r.cur.cache[v.ID] = v
	}
	for _, v := range queued {
		// Queued tasks may arrive with StateActive (a task enters live
		// concurrently); mirror the prior Frame's clamp so queued lines show
		// grey/dim, not yellow.
		vc := v
		if vc.State == download.StateActive {
			vc.State = download.StateQueued
		}
		r.cur.cache[vc.ID] = vc
	}

	// Promote vanished tasks to their terminal state. Build the set of ids
	// present this frame; anything in the cache not in it has retired.
	present := make(map[download.TaskID]struct{}, len(live)+len(queued))
	for _, v := range live {
		present[v.ID] = struct{}{}
	}
	for _, v := range queued {
		present[v.ID] = struct{}{}
	}
	for id, v := range r.cur.cache {
		if _, ok := present[id]; ok {
			continue
		}
		// Never promote a still-active/queued task just because its snapshot
		// skipped a frame; only retire entries that actually look finished.
		if v.State == download.StateCompleted || v.State == download.StateError {
			continue // already terminal — leave as-is
		}
		if v.TotalSize > 0 && v.BytesDone >= v.TotalSize {
			v.State = download.StateCompleted
			v.Connections = 0
			v.Speed = 0
			v.ETA = 0
			r.cur.cache[id] = v
		} else if v.TotalSize <= 0 {
			// Unknown-size task that left the live set: we can't prove 100%,
			// so leave its last (active) colour but stop it from counting as
			// incomplete forever — it's no longer downloading. Mark completed:
			// the sizeless bar paints green and isn't held against the ETA.
			v.State = download.StateCompleted
			v.Connections = 0
			v.Speed = 0
			v.ETA = 0
			r.cur.cache[id] = v
		}
	}

	r.cur.live = live
	r.cur.queue = queued
	return r.cur.ordered()
}

// computedView folds a snapshot for the summary: a completed task contributes
// (state, bytes) without speed/ETA churn so Total/speed/ETA reflect only work
// still in flight.
type viewStats struct {
	completed, total int
	speed            int64
	maxETA           time.Duration
	bytesDone        int64
	totalSize        int64
}

// aggregate computes the bottom summary from the ordered view, counting by
// *unique id* (not len(live)+len(queued)+completed) so a completed task still
// transit in `live` for a frame can't double-count (bug §3.2). Total = number
// of distinct ids seen; Completed = those whose state is StateCompleted.
func aggregate(view []download.ProgressView) viewStats {
	ids := make(map[download.TaskID]struct{}, len(view))
	var st viewStats
	for _, v := range view {
		if _, ok := ids[v.ID]; ok {
			continue
		}
		ids[v.ID] = struct{}{}
		st.total++
		st.bytesDone += v.BytesDone
		if v.TotalSize > 0 {
			st.totalSize += v.TotalSize
		}
		if v.State == download.StateCompleted {
			st.completed++
		}
		// Speed/ETA only from tasks still doing work.
		if v.State != download.StateCompleted {
			st.speed += v.Speed
			if v.ETA > st.maxETA {
				st.maxETA = v.ETA
			}
		}
	}
	return st
}

// isActive reports whether a task should be rendered in the per-file list.
// Tasks that haven't started producing data yet (0 bytes, 0 speed) are hidden
// to avoid cluttering the display with zombie lines that flash briefly — but a
// task that reached a terminal state IS shown even at 0 bytes, otherwise an
// empty file (or a failed probe) would vanish from the list while still
// counting toward the summary.
func isActive(v download.ProgressView) bool {
	if v.State == download.StateCompleted || v.State == download.StateError {
		return true
	}
	return v.BytesDone != 0 || v.Speed != 0
}

// Frame renders the full set of task lines + summary, overwriting the previous
// frame's lines in place (TTY) or emitting a throttled periodic log line
// (non-TTY). `live` and `queued` snapshots come from the Scheduler's ProgressCB.
//
// On a TTY every line is truncated to the terminal width minus 1 (rune-safe) so
// one logical line never wraps to two physical rows — the cursor-up count we
// replay next frame then always matches rows on screen (bug §3.3). Width is
// re-read per frame so a resize mid-batch is picked up.
func (r *Renderer) Frame(live, queued []download.ProgressView) {
	if r.quiet {
		return
	}

	view := r.updateCache(live, queued)
	st := aggregate(view)

	// Non-TTY: throttle. Skip frames that aren't a milestone. (Nil-nil final
	// frame from main.go is always emitted so the redirected log ends on the
	// true totals, not the last throttled midpoint.)
	if !r.tty {
		r.emitNonTTY(view, st, live == nil && queued == nil)
		return
	}

	// TTY: build lines, truncated to width so wrapping can't desync cursor-up.
	// Width AND height are re-read per frame: a mid-run resize (usually via
	// SIGWINCH waking the ticker) is picked up on the very next frame.
	width, height := r.sizeFn(r.w)
	// Layered degradation for narrow terminals: full columns down to 96 cols,
	// speed/ETA dropped below 66, bar dropped below 45; under that the line is
	// name + percent only.
	barW, nameWidth, _, layout := layoutFor(width)
	lines := make([]string, 0, len(view)+1)
	hidden := 0
	pos := bouncePosition(r.indeterminateTick, barW)
	// Row budget: keep the task list + summary (+ the "N more…" line when
	// tasks are cut off) inside the terminal height so the cursor-up count
	// always matches physical rows. A batch taller than the screen used to
	// scroll the list and corrupt the next redraw.
	rowBudget := height
	if rowBudget < 1 {
		rowBudget = 1
	}
	if st.total > 0 {
		rowBudget-- // reserve one row for the summary
	}
	rowBudget-- // reserve one row for the "N more…" line when needed
	if rowBudget < 0 {
		rowBudget = 0
	}
	for _, v := range view {
		if !isActive(v) {
			continue
		}
		if len(lines) >= rowBudget {
			hidden++
			continue
		}
		line := renderTaskLine(v, r.useColor, sizelessPos(v, pos), r.indeterminateTick, nameWidth, barW, layout)
		lines = append(lines, truncateToWidth(line, width))
	}
	// Only show summary when there are tasks — avoids the misleading
	// "Total: 0/0" before any snapshots arrive.
	if st.total > 0 {
		lines = append(lines, truncateToWidth(RenderSummaryWidth(st.completed, st.total, st.speed, st.maxETA, st.bytesDone, st.totalSize, r.useColor, width, barW), width))
	}
	// When tasks were cut off the visible list, say so instead of leaving the
	// user wondering where the rest went. ANSI-free so the width math holds.
	if hidden > 0 {
		lines = append(lines, truncateToWidth(fmt.Sprintf("%d more…", hidden), width))
	}

	// Move cursor up over the previous frame, clearing each row.
	for i := 0; i < r.lastLines; i++ {
		fmt.Fprint(r.w, ansiCursorUp+ansiClearLine)
	}
	for _, l := range lines {
		fmt.Fprintln(r.w, l)
	}
	r.lastLines = len(lines)
	r.indeterminateTick++
}

// sizelessPos returns the bounce position for a task's indeterminate bar, or -1
// (static centred) when the task has a known size. Only sizeless bars animate.
func sizelessPos(v download.ProgressView, pos int) int {
	if v.TotalSize <= 0 {
		return pos
	}
	return -1
}

// layoutFor picks the per-file line layout for a terminal width (display
// cells). The bar shrinks first (existing behaviour), then whole columns
// drop — speed/ETA, then size/conn — down to an absolute floor of just the
// name and the percent. Every tier still renders a single non-wrapping row;
// the previous code kept all five fixed columns even at 40 columns, where the
// name got squeezed to its 10-cell minimum and the pct risked being truncated
// off the edge. minW is the narrowest the line can be for the tier; it's the
// math the tier selection is built on, and ≤ termWidth always.
func layoutFor(termWidth int) (barW, nameW, minW int, layout lineLayout) {
	switch {
	case termWidth >= 96:
		barW = BarWidth
		nameW = termWidth - infoBlockWidthFor(barW) - 1
		minW = infoFixedWidth + barW + 1 // " name  <info block>"
		layout = layoutFull
	case termWidth >= 66:
		barW = max(minBarWidth, BarWidth-10)
		nameW = 8
		minW = infoFixedWidth - (colSpeed + colETA + 4) + barW + 2 // no speed/ETA columns
		layout = layoutNoSpeedETA
	case termWidth >= 45:
		barW = minBarWidth
		nameW = 6
		minW = infoFixedWidth - (colSpeed + colETA + colSize + 4) + 2 + barW + 2 // name + bar + pct
		layout = layoutNameBarPct
	default: // <45: barless; name + pct only
		barW = 0
		nameW = max(termWidth-6, 6)
		minW = 4
		layout = layoutNamePct
	}
	if nameW < 4 {
		nameW = 4
	}
	return
}

// truncateToWidth cuts s to at most width visible display cells so it fits on
// one terminal row without wrapping. ANSI escape sequences are preserved
// intact — only visible characters are counted toward the width limit. A
// width<=0 means "unbounded" and returns s unchanged.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	if displayWidth(s) <= width {
		return s
	}
	return truncateVisibleWidth(s, width)
}

// emitNonTTY prints the non-TTY snapshot subject to the milestone+interval
// throttle (PRD §8.2: "periodic log lines... every 10% or every fixed time
// interval"). `final` forces a full flush regardless of elapsed time — that's
// the post-Run Frame(nil,nil) call, so a redirected log always ends on the
// true final totals and the per-file lines.
//
// Two release triggers beyond `final`:
//   - milestone: the count of completed tasks ticked up (a task just finished).
//     This bypasses the interval gate and emits the full per-file block — a
//     reader of the redirected log sees each file land at 100%.
//   - interval: ~2s elapsed since the last flush. Emits the summary line only
//     (no per-file block) so a long in-flight stretch is summarised without
//     reprinting every file every interval.
//
// Pure byte ticks (file downloading, no completion) never bypass the gate:
// the milestone key is just the completed-count, so flowing bytes coalesce
// under the interval rather than producing a line every 100ms.
func (r *Renderer) emitNonTTY(view []download.ProgressView, st viewStats, final bool) {
	now := nowFn()

	// Milestone key = (#completed). A byte-only change keeps the same key, so
	// it's gated by the interval; a real completion (or a task entering the
	// view) changes it and flushes immediately.
	key := fmt.Sprintf("%d|%d", st.completed, st.total)
	intervalElapsed := !r.lastNonTTYFlush.IsZero() && now.Sub(r.lastNonTTYFlush) >= r.nonTTYInterval
	milestone := key != r.lastLoggedDoneKey

	if !final && !intervalElapsed && !milestone {
		return
	}

	// Decide whether to print the full per-file block (milestone/final) or
	// just the summary (coalesced in-flight interval tick).
	full := final || milestone

	r.lastNonTTYFlush = now
	r.lastLoggedDoneKey = key
	if st.total > 0 {
		r.lastLoggedPct = min(int(float64(st.completed)/float64(st.total)*100), 100)
	}

	summary := RenderSummary(st.completed, st.total, st.speed, st.maxETA, st.bytesDone, st.totalSize, r.useColor)
	var b strings.Builder
	if full {
		b.WriteString("---\n")
		for _, v := range view {
			if !isActive(v) {
				continue
			}
			b.WriteString(renderTaskLine(v, r.useColor, -1, r.indeterminateTick, 40, BarWidth, layoutFull))
			b.WriteByte('\n')
		}
		b.WriteString(summary)
		b.WriteByte('\n')
	} else {
		b.WriteString(summary)
		b.WriteByte('\n')
	}
	fmt.Fprint(r.w, b.String())
}

// RunLoop drives the renderer off a snapshot channel until ctx is cancelled.
// interval is the redraw cadence (~100ms; PRD §11.1). On ctx cancel the loop
// emits one final frame from whatever was last seen so the terminal lands on
// the completed bars rather than a halfway snapshot.
func (r *Renderer) RunLoop(ctx context.Context, interval time.Duration,
	snapshots <-chan []download.ProgressView, qSnapshots <-chan []download.ProgressView,
) {
	r.Begin()
	defer r.End()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final frame from cache: empty live+queued re-emits retained state.
			r.Frame(nil, nil)
			return
		case s := <-snapshots:
			r.cur.live = s
			r.Frame(r.cur.live, r.cur.queue)
		case s := <-qSnapshots:
			r.cur.queue = s
			r.Frame(r.cur.live, r.cur.queue)
		case <-t.C:
			// Re-render last-seen slices; updateCache reconciles the cache and
			// advances the indeterminate animation even when nothing new came.
			r.Frame(r.cur.live, r.cur.queue)
		case <-r.Wake:
			// Terminal resize (SIGWINCH) or any external nudge: re-render
			// immediately so the new size takes effect on this frame.
			r.Frame(r.cur.live, r.cur.queue)
		}
	}
}
