// Package ui renders ODM's pacman/CachyOS (ILoveCandy) progress bar,
// runs the live TTY redraw loop over a snapshot feed, and shows the
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
	"sync"
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
// when NO_COLOR isn't set.
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
	// it visibly moves between ticks (bug). Reset for nothing — a missed
	// frame just delays the bounce, never corrupts state.
	indeterminateTick int

	// startedAt is when RunLoop began, so the summary can show an elapsed
	// counter ("+HH:MM:SS"). Zero when Frame is driven directly (tests,
	// non-TTY probes) — the elapsed segment is omitted then.
	startedAt time.Time

	// Wake is a notification channel the RunLoop drains to re-render
	// immediately. main.go pings it on SIGWINCH so a terminal resize lands on
	// the next frame instead of waiting out the 100ms tick. Buffer 1 and
	// non-blocking send: coalesced, never blocks the sender.
	Wake chan struct{}

	// Non-TTY throttle: when stdout is redirected we don't want a
	// line every 100ms. We print at most one summary snapshot per
	// nonTTYInterval, and additionally whenever the aggregate state changes
	// (a task completes/pauses, or the batch's aggregate percentage crosses an
	// integer threshold) so a reader of the log still sees milestones.
	nonTTYInterval  time.Duration
	lastNonTTYFlush time.Time
	// lastLoggedDoneKey is a cheap fingerprint of (count completed, sum done)
	// so a state change with no byte progress doesn't re-emit a near-identical
	// line every interval.
	lastLoggedDoneKey string

	// mu serialises every stdout write (Frame, Begin, End, Interject). The
	// engine logs through InterjectWriter from task goroutines while RunLoop
	// frames from its own — without the mutex their escape sequences would
	// interleave mid-sequence and shred the frame.
	mu sync.Mutex
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
		// "every 10% or every fixed time interval". One line/2s is
		// readable in a redirected log without drowning it; the state-change
		// path adds milestone lines on top.
		nonTTYInterval: 2 * time.Second,
	}
}

// Begin hides the cursor (TTY mode).
func (r *Renderer) Begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tty {
		fmt.Fprint(r.w, ansiCursorHide)
	}
}

// End shows the cursor again + trailing newline so the shell prompt lands below.
// The newline consumes one screen row, so lastLines grows by one to match: the
// caller's next Frame must cursor-up over the blank row too, or the redraw
// lands one row low and the top row of the previous frame survives on screen
// (the duplicated task line seen after ^C).
func (r *Renderer) End() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tty {
		fmt.Fprint(r.w, ansiCursorShow)
		r.lastLines++
	}
	fmt.Fprintln(r.w)
}

// Live reports whether the renderer owns a TTY screen (interactive run, not
// quiet). The CLI uses it to decide whether engine logs should be routed
// through InterjectWriter.
func (r *Renderer) Live() bool { return r.tty && !r.quiet }

// InterjectWriter returns an io.Writer that prints each Write (a logging
// package emits one entry per Write, newline included) through Renderer.
// Interject instead of raw stderr — see Interject for why that matters.
func InterjectWriter(r *Renderer) io.Writer { return interruptWriter{r} }

type interruptWriter struct{ r *Renderer }

func (w interruptWriter) Write(p []byte) (int, error) {
	w.r.Interject(string(p))
	return len(p), nil
}

// Interject prints an out-of-band line (engine log output) without corrupting
// the live frame: erase the frame rows, print the line, redraw the frame
// underneath. A bare write between two frames shifts the terminal cursor, so
// the next Frame's cursor-up under-counts and stale rows survive above the
// new frame — the duplicated task lines seen on ^C. Routing every engine log
// through here keeps the renderer the single owner of the screen.
func (r *Renderer) Interject(line string) {
	line = strings.TrimRight(line, "\n")
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quiet {
		return
	}
	if !r.tty {
		fmt.Fprintln(r.w, line)
		return
	}
	// \r first: cursor-up preserves the column, so the erase must start from a
	// known column. If any foreign write ever left the cursor mid-line, the
	// redraw would otherwise begin glued to stale row tails.
	fmt.Fprint(r.w, "\r")
	for range r.lastLines {
		fmt.Fprint(r.w, ansiCursorUp+ansiClearLine)
	}
	r.lastLines = 0
	fmt.Fprintln(r.w, line)
	redrawn := r.composeLines(r.cur.ordered(), aggregate(r.cur.ordered()))
	for _, l := range redrawn {
		fmt.Fprintln(r.w, l)
	}
	r.lastLines = len(redrawn)
}

// updateCache merges the frame's live + queued snapshots into r.cur.cache and
// refreshes the cached live/queue slices. It returns the full ordered view to
// render (live, retained-retired, queued). Two rules fix the headline bug:
//
//  1. A task present in the cache but absent from BOTH live and queued is
//     treated as retired: we promote it to its terminal state — Completed
//     (green) when bytesDone>=totalSize or the size was unknown, else we keep
//     its last state. This is the "vanished completed line must stay on screen
//     at 100%" fix (bug): the scheduler deletes a finished task from its
//     live map the instant it completes and never forwards the terminal
//     snapshot, so without retention the bar blinks to zero and the summary
//     reads Total: 0/0.
//  2. nil live+queued (the post-Run "final frame" call from main.go) is a
//     request to re-render from cache, not to clear it — Frame(nil,nil) must
//     still show the completed batch.
func (r *Renderer) updateCache(live, queued []download.ProgressView) []download.ProgressView {
	if live == nil && queued == nil {
		// Final-frame call: keep cache as-is and re-emit — with one repair.
		// The run is over (main only issues this after the scheduler has
		// returned), but the terminal snapshot of a task can still be sitting
		// unconsumed in the progress channel when the loop tears down (select
		// picked ctx.Done over the buffered send). A cached view holding all
		// its bytes IS finished, whatever stale state it carries; without this
		// the last screen shows an active-at-100% row and "Total: 0/N".
		for id, v := range r.cur.cache {
			if v.State == download.StateCompleted || v.State == download.StateError {
				continue
			}
			if v.TotalSize > 0 && v.BytesDone >= v.TotalSize {
				v.State = download.StateCompleted
				v.Connections = 0
				v.Speed = 0
				v.ETA = 0
				r.cur.cache[id] = v
			}
		}
		// Drop the last-seen slices too: ordered() prefers them over the
		// cache, so a stale active entry there would shadow the promoted one.
		r.cur.live = nil
		r.cur.queue = nil
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
// transit in `live` for a frame can't double-count (bug). Total = number
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
// replay next frame then always matches rows on screen (bug). Width is
// re-read per frame so a resize mid-batch is picked up.
func (r *Renderer) Frame(live, queued []download.ProgressView) {
	if r.quiet {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	view := r.updateCache(live, queued)
	st := aggregate(view)

	// Non-TTY: throttle. Skip frames that aren't a milestone. (Nil-nil final
	// frame from main.go is always emitted so the redirected log ends on the
	// true totals, not the last throttled midpoint.)
	if !r.tty {
		r.emitNonTTY(view, st, live == nil && queued == nil)
		return
	}

	lines := r.composeLines(view, st)
	// Move cursor up over the previous frame, clearing each row. \r first so
	// the erase starts from a known column even if a foreign write ever left
	// the cursor mid-line.
	fmt.Fprint(r.w, "\r")
	for i := 0; i < r.lastLines; i++ {
		fmt.Fprint(r.w, ansiCursorUp+ansiClearLine)
	}
	for _, l := range lines {
		fmt.Fprintln(r.w, l)
	}
	r.lastLines = len(lines)
	r.indeterminateTick++
}

// composeLines builds one frame's rows for a TTY: per-file task lines plus
// the aggregate summary, truncated to terminal width so wrapping can't desync
// cursor-up accounting. Width AND height are re-read per frame: a mid-run
// resize is picked up on the very next frame.
func (r *Renderer) composeLines(view []download.ProgressView, st viewStats) []string {
	width, height := r.sizeFn(r.w)
	// Layered degradation for narrow terminals: full columns down to 96 cols,
	// speed/ETA dropped below 66, bar dropped below 45; under that the line is
	// name + percent only.
	barW, nameWidth, _, layout := layoutFor(width)
	lines := make([]string, 0, len(view)+1)
	hidden := 0
	pos := bouncePosition(r.indeterminateTick, barW)
	elapsed := time.Duration(0)
	if !r.startedAt.IsZero() {
		elapsed = nowFn().Sub(r.startedAt)
	}
	// Row budget: keep the task list + summary (+ the "N more..." line when
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
	rowBudget-- // reserve one row for the "N more..." line when needed
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
		line := renderTaskLine(v, r.useColor, sizelessPos(v, pos), nameWidth, barW, layout)
		lines = append(lines, truncateToWidth(line, width))
	}
	// Only show summary when there are tasks — avoids the misleading
	// "Total: 0/0" before any snapshots arrive.
	if st.total > 0 {
		lines = append(lines, truncateToWidth(RenderSummaryWidth(st.completed, st.total, st.speed, st.maxETA, elapsed, st.bytesDone, st.totalSize, r.useColor, width, barW), width))
	}
	// When tasks were cut off the visible list, say so instead of leaving the
	// user wondering where the rest went. ANSI-free so the width math holds.
	if hidden > 0 {
		lines = append(lines, truncateToWidth(fmt.Sprintf("%d more...", hidden), width))
	}
	return lines
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
// drop — speed/ETA, then size/conn — down to a name+pct floor and finally a
// pct-only line, so even a 10-column terminal gets a usable (non-wrapping)
// row. Every tier still renders a single row; the previous code kept all five
// fixed columns even at 40 columns, where the name got squeezed to its
// 6-cell minimum and the pct risked being truncated off the edge. minW is the
// narrowest the line can be for the tier; it's the math the tier selection is
// built on, and ≤ termWidth always.
func layoutFor(termWidth int) (barW, nameW, minW int, layout lineLayout) {
	switch {
	case termWidth >= 96:
		barW = BarWidth
		nameW = termWidth - infoBlockWidthFor(barW) - 1
		minW = colGlyph + infoFixedWidth + barW + 1
		layout = layoutFull
	case termWidth >= 66:
		barW = max(minBarWidth, BarWidth-10)
		nameW = termWidth - (colGlyph + 2 + colSize + 2 + colConns + 1 + barW + 1 + 2 + colPct) - 1
		minW = colGlyph + (2 + colSize + 2 + colConns + 1 + 1 + 2 + colPct) + barW + 1
		layout = layoutNoSpeedETA
	case termWidth >= 36:
		barW = minBarWidth
		nameW = termWidth - (colGlyph + 2 + colConns + 1 + barW + 1 + 2 + colPct) - 1
		minW = colGlyph + (2 + colConns + 1 + 1 + 2 + colPct) + barW + 1
		layout = layoutNameBarPct
	case termWidth >= 12:
		barW = 0
		nameW = termWidth - (colGlyph + 2 + colPct) - 1
		layout = layoutNamePct
	default: // <12: the percent alone, right-aligned — the super-narrow floor
		barW = 0
		nameW = max(termWidth-1, 1) // the pct is fitted into this many cells
		layout = layoutPct
	}
	if nameW < 4 && layout != layoutPct {
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
// throttle ("periodic log lines... every 10% or every fixed time interval").
// `final` forces a full flush regardless of elapsed time — that's
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

	// Elapsed counter for the summary (RunLoop only; 0 when Frame is driven
	// directly).
	elapsed := time.Duration(0)
	if !r.startedAt.IsZero() {
		elapsed = nowFn().Sub(r.startedAt)
	}
	summary := RenderSummary(st.completed, st.total, st.speed, st.maxETA, elapsed, st.bytesDone, st.totalSize, r.useColor)
	var b strings.Builder
	if full {
		b.WriteString("---\n")
		for _, v := range view {
			if !isActive(v) {
				continue
			}
			b.WriteString(renderTaskLine(v, r.useColor, -1, 40, BarWidth, layoutFull))
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
// interval is the redraw cadence (~100ms). On ctx cancel the loop
// emits one final frame from whatever was last seen so the terminal lands on
// the completed bars rather than a halfway snapshot.
//
// RunLoop synchronises its exit through done: when it returns (either from ctx
// cancellation or from a drained snapshot stream), done is closed. The caller
// MUST wait on done before driving any further Frame() calls — otherwise two
// goroutines (the loop's final frame and the caller's post-run Frame) race on
// the same stdout and interleave their cursor-up/clear sequences, producing
// the doubled/truncated "chaos" seen on ^C.
func (r *Renderer) RunLoop(ctx context.Context, interval time.Duration,
	snapshots <-chan []download.ProgressView, qSnapshots <-chan []download.ProgressView,
	done chan<- struct{},
) {
	r.startedAt = nowFn()
	r.Begin()
	// Defers run LIFO: End() must execute BEFORE close(done) — main waits on
	// done and then immediately draws the final frame, so the cursor restore
	// has to be finished first or the two writers interleave their escape
	// sequences (the doubled lines seen on ^C).
	defer close(done)
	defer r.End()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Do NOT render a final frame here: the caller (main) owns the one
			// final frame, issued AFTER this loop has fully exited (done is
			// closed only after End() restores the cursor). Rendering here too
			// would give two writers of the final frame and the doubled,
			// interleaved lines seen on ^C. The caller's Frame(nil,nil) renders
			// the retained cache as the single final screen.
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
