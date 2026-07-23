// SPDX-License-Identifier: MIT
//
// render.go holds the pure pacman-bar formatting the §8 PRD defines: Bar(),
// RenderTaskLine(), RenderSummary() and the humanising helpers. These are the
// functions progress_test.go pins down by signature, so the visual layout
// (c/o/-, [x<N>], <percent>% and the summary line) stays byte-identical to the
// previous build for the deterministic cases; only the bug-driven behaviours
// change:
//
//   - filename truncation is rune-safe (no mid-codepoint cuts on UTF-8 names)
//   - the indeterminate (sizeless) bar animates position-bounce across frames
//     instead of parking pacman dead-centre forever
//
// The animated indeterminate position is passed in by Bar's caller (the
// Renderer owns the wall-clock tick); Bar itself stays pure so the existing
// "X% eaten => N dashes + c + rest dots" math is unchanged and still testable.

package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"odm/internal/download"
)

// BarWidth is the visual width of the pacman bar (in cells).
const BarWidth = 30

// pacFace is the pacman icon — the "c" from ILoveCandy in the original Arch
// animation; rendered as "c" per the PRD §8 spec text.
const pacFace = "c"

// Color is a state.colour pair (foreground). "" → no colour (non-TTY).
type Color string

const (
	colorReset  Color = "\x1b[0m"
	colorGreen  Color = "\x1b[32m"
	colorYellow Color = "\x1b[33m"
	colorRed    Color = "\x1b[31m"
	colorGrey   Color = "\x1b[90m"
)

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

// FormatDuration turns a time.Duration into MM:SS (the bar's <ETA>). A
// nonsensically large ETA — produced by estimateETA when the rolling speed
// has decayed near zero mid-stream — is clamped and rendered as "--:--" rather
// than overflowing to "30744118:48" (~58 years), which would shatter the
// per-line column layout the width-aware redraw relies on. The cap is generous
// (just over a day) so any real single-download ETA still fits in MM:SS while
// garbage from an EMA bottoming out is masked.
func FormatDuration(d time.Duration) string {
	if d <= 0 || d > 36*time.Hour {
		return "--:--"
	}
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// Bar renders one pacman progress bar into a fixed-width string. `done`/`total`
// are done/total bytes; we compute the fraction eaten and draw:
//
//	eaten region  → "-" (blank/dashes)
//	pacman face   → "c"
//	remaining     → "o"
//
// At 100% the whole bar is dashes/blank (PRD §8 "nothing left for pacman to
// eat"). The extras over the previous build:
//   - the indeterminate (total<=0) bar animates: pos is the bounce position
//     [0..width) the Renderer advances per frame (see tickIndeterminate). When
//     pos<0 the bar falls back to the static centre layout Bar() stored before
//     (kept for callers/tests that don't drive the tick).
func Bar(done, total int64, width int) string {
	return BarIndeterminate(done, total, width, -1)
}

// BarIndeterminate is Bar with an explicit pacman position for the sizeless
// (indeterminate) case. pos==-1 means "no animation slot provided" and the
// static centred layout is used (back-compat with the pure Bar() contract the
// tests pin). pos is clamped to [0,width-1].
func BarIndeterminate(done, total int64, width int, pos int) string {
	if width < 2 {
		width = 2
	}
	if total <= 0 {
		// Sizeless stream: indeterminate. If the caller passed a live bounce
		// position we honour it; otherwise pacman parks at the middle for
		// callers/tests that ask for a single static frame.
		if pos < 0 {
			half := width / 2
			return strings.Repeat("-", half) + pacFace + strings.Repeat("o", width-half-1)
		}
		if pos >= width-1 {
			pos = width - 1
		}
		if pos < 0 {
			pos = 0
		}
		// Eaten (dashes) on the left of pacman, dots (uneaten) on the right —
		// the same eaten/face/dot shape as the sized case so the visual reads
		// identically, only the mouth now travels back and forth.
		return strings.Repeat("-", pos) + pacFace + strings.Repeat("o", width-pos-1)
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

// maxNameRunes caps the displayed filename to keep one task line at a sane
// width. The limit is in runes (display width), not bytes — see truncateName.
const maxNameRunes = 20

// truncateName cuts name to maxNameRunes display columns, rune-safe: a UTF-8
// name (CJK, emoji, etc.) is split on codepoint boundaries, never mid-byte, so
// no mojibake lands on screen (bug §3.4). An "…" ellipsis occupies the last 3
// runes when trimmed.
func truncateName(name string) string {
	if utf8.RuneCountInString(name) <= maxNameRunes {
		return name
	}
	rs := []rune(name)
	return string(rs[:maxNameRunes-3]) + "..."
}

// RenderTaskLine formats one per-file line per PRD §8.1, colouring it when
// useColor is true. The line layout:
//
//	<file_name>   <size>   <speed>/s   <ETA>   [x<N>]   [<bar>]   <percent>%
//
// indeterminatePos drives the sizeless bar animation; pass -1 for a static
// (centred) indeterminate bar.
func RenderTaskLine(v download.ProgressView, useColor bool) string {
	return renderTaskLine(v, useColor, -1)
}

func renderTaskLine(v download.ProgressView, useColor bool, indeterminatePos int) string {
	name := v.Filename
	if name == "" {
		name = v.URL
	}
	name = truncateName(name)
	size := FormatFileSize(v.TotalSize)
	if v.TotalSize < 0 {
		size = "?"
	}
	speed := FormatSpeed(v.Speed)
	eta := FormatDuration(v.ETA)
	conns := v.Connections
	bar := BarIndeterminate(v.BytesDone, v.TotalSize, BarWidth, indeterminatePos)
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
