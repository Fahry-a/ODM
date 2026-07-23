// Package ui renders ODM's pacman/CachyOS (ILoveCandy) progress bar (PRD §8),
// runs the live TTY redraw loop over a snapshot feed, and shows the §9
// confirmation prompt. It is the only strconv away-printing layer of the
// engine — the download + scheduler packages send ProgressView snapshots.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"odm/internal/download"
)

// ANSI control sequences we use.
const (
	ansiClearLine    = "\x1b[2K"
	ansiCursorUp     = "\x1b[A"
	ansiCursorHide   = "\x1b[?25l"
	ansiCursorShow   = "\x1b[?25h"
	ansiCursorToLine = "\x1b[%d;1H" // not used (we move relatively)
)

// Color is a state.colour pair (foreground). "" → no colour (non-TTY).
type Color string

const (
	colorReset  Color = "\x1b[0m"
	colorGreen  Color = "\x1b[32m"
	colorYellow Color = "\x1b[33m"
	colorRed    Color = "\x1b[31m"
	colorGrey   Color = "\x1b[90m"
)

// BarWidth is the visual width of the pacman bar (in cells).
const BarWidth = 30

// pacFace is the pacman icon — actual glyph in the original Arch animation is
// the "c" from ILoveCandy; we render it as "c" per the PRD §8 spec text.
const pacFace = "c"

// stateColor maps a TaskState to its §8 colour.
func stateColor(s download.TaskState, useColor bool) Color {
	if !useColor {
		return ""
	}
	switch s {
	case download.StateCompleted:
		return colorGreen
	case download.StateActive:
		return colorYellow
	case download.StateRetrying, download.StateError:
		return colorRed
	case download.StateQueued, download.StatePaused:
		return colorGrey
	}
	return ""
}

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

// FormatFileSize humanises a byte count (binary, KiB/MiB/GiB/…).
func FormatFileSize(b int64) string {
	const unit = 1024.0
	if b < 0 {
		return "?"
	}
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	val := float64(b)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	idx := -1
	for val >= unit && idx < len(units)-1 {
		val /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", val, units[idx])
}

// FormatSpeed humanises bytes/sec.
func FormatSpeed(bps int64) string {
	if bps < 0 {
		return "--"
	}
	return FormatFileSize(bps) + "/s"
}

// FormatDuration turns a time.Duration into MM:SS (the bar's <ETA>).
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "--:--"
	}
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// Bar renders one pacman progress bar into a fixed-width string. `i`/`total`
// are done/total bytes; we compute the fraction eaten and draw:
//
//	eaten region  → "-" (blank/dashes)
//	pacman face   → "c"
//	remaining     → "o"
//
// At 100% the whole bar is blank/dashes (PRD §8 "there's nothing left for pacman to eat").
func Bar(done, total int64, width int) string {
	if width < 2 {
		width = 2
	}
	if total <= 0 {
		// Sizeless stream: indeterminate — pacman sits near the middle and we
		// show a half-eaten trailing bar of dots.
		half := width / 2
		return strings.Repeat("-", half) + pacFace + strings.Repeat("o", width-half-1)
	}
	if done >= total {
		return strings.Repeat("-", width) // fully eaten
	}
	frac := float64(done) / float64(total)
	eaten := int(frac * float64(width))
	if eaten >= width {
		eaten = width - 1
	}
	// `eaten` cells already eaten → dashes; then the face; then the rest dots.
	return strings.Repeat("-", eaten) + pacFace + strings.Repeat("o", width-eaten-1)
}

// RenderTaskLine formats one per-file line per PRD §8.1, colouring it when
// useColor is true. The line layout:
//
//	<file_name>   <size>   <speed>/s   <ETA>   [x<N>]   [<bar>]   <percent>%
func RenderTaskLine(v download.ProgressView, useColor bool) string {
	name := v.Filename
	if name == "" {
		name = v.URL
	}
	if len(name) > 20 {
		name = name[:17] + "..."
	}
	size := FormatFileSize(v.TotalSize)
	if v.TotalSize < 0 {
		size = "?"
	}
	speed := FormatSpeed(v.Speed)
	eta := FormatDuration(v.ETA)
	conns := v.Connections
	bar := Bar(v.BytesDone, v.TotalSize, BarWidth)
	pct := 0
	if v.TotalSize > 0 {
		pct = min(max(int(float64(v.BytesDone)/float64(v.TotalSize)*100), 0), 100)
	}
	c := stateColor(v.State, useColor)
	reset := ""
	if c != "" {
		reset = string(colorReset)
	}
	return fmt.Sprintf("%s%-20s  %6s  %11s  %5s  [x%d]  [%s]  %3d%%%s",
		c, name, size, speed, eta, conns, bar, pct, reset)
}

// RenderSummary formats the bottom summary line (PRD §8.1 example):
//
//	Total: X/Y completed  |  <speed>/s  |  ETA HH:MM:SS
func RenderSummary(completed, total int, speedBps int64, eta time.Duration, useColor bool) string {
	sp := FormatSpeed(speedBps)
	return fmt.Sprintf("Total: %d/%d completed  |  %s  |  ETA %s", completed, total, sp, FormatDuration(eta))
}

// Renderer owns the live redraw state: how many lines were last written so the
// next frame can move the cursor up and overwrite in place (ANSI cursor
// control), and a flag for non-TTY fallback.
type Renderer struct {
	w        io.Writer
	useColor bool
	tty      bool
	lastLines int
	quiet    bool
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

// Frame renders the full set of live task lines + summary, overwriting the
// previous frame's lines in place (TTY) or emitting a periodic log line
// (non-TTY). `live` and `queued` snapshots come from the Scheduler.
func (r *Renderer) Frame(live, queued []download.ProgressView) {
	lines := make([]string, 0, len(live)+len(queued)+1)

	if r.tty {
		// move cursor up over previous frame.
		for i := 0; i < r.lastLines; i++ {
			fmt.Fprint(r.w, ansiCursorUp+ansiClearLine)
		}
	} else if r.quiet {
		return
	}

	// Live lines (downloading + retrying + error + paused), then queued (grey).
	for _, v := range live {
		lines = append(lines, RenderTaskLine(v, r.useColor))
	}
	for _, v := range queued {
		vcopy := v
		if vcopy.State == download.StateActive {
			vcopy.State = download.StateQueued
		}
		lines = append(lines, RenderTaskLine(vcopy, r.useColor))
	}

	completed := 0
	var speed int64
	var maxETA time.Duration
	for _, v := range live {
		if v.State == download.StateCompleted {
			completed++
		}
		speed += v.Speed
		if v.ETA > maxETA && v.ETA > 0 {
			maxETA = v.ETA
		}
	}
	total := len(live) + len(queued) + completed
	lines = append(lines, RenderSummary(completed, total, speed, maxETA, r.useColor))

	for _, l := range lines {
		fmt.Fprintln(r.w, l)
	}
	r.lastLines = len(lines)

	if !r.tty && !r.quiet {
		// Non-TTY fallback: leave a blank separator so each frame is readable
		// (PRD §8.2: periodic log lines, no ANSI cursor control).
		fmt.Fprintln(r.w)
	}
}

// RunLoop drives the renderer off a snapshot channel until ctx is cancelled.
// interval is the redraw cadence (~100ms; PRD §11.1 suggests throttling).
func (r *Renderer) RunLoop(ctx context.Context, interval time.Duration,
	snapshots <-chan []download.ProgressView, qSnapshots <-chan []download.ProgressView,
) {
	r.Begin()
	defer r.End()
	t := time.NewTicker(interval)
	defer t.Stop()
	var live, queued []download.ProgressView
	for {
		select {
		case <-ctx.Done():
			if len(live) == 0 && len(queued) == 0 {
				return
			}
			r.Frame(live, queued)
			return
		case s := <-snapshots:
			live = s
		case s := <-qSnapshots:
			queued = s
		case <-t.C:
			r.Frame(live, queued)
		}
	}
}
