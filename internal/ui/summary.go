// SPDX-License-Identifier: MIT
//
// summary.go holds the bottom aggregate summary line (RenderSummary*), the
// byte/speed/duration humanisers, and the per-state status glyph + colour
// mapping. These are pure formatting helpers the task line and the summary
// share; keeping them apart from the task-line layout (render.go) makes the
// column-width budget a single place to reason about.
package ui

import (
	"fmt"
	"strings"
	"time"

	"odm/internal/download"
)

// FormatFileSize humanises a byte count (binary, KiB/MiB/GiB/…).
// Rounded values never print as "1024.0 XiB" — that would overflow colSize
// and re-introduce the column jitter the fixed layout is meant to kill.
func FormatFileSize(b int64) string {
	const unit = 1024.0
	if b < 0 {
		return unknownGlyph
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
	// %.1f rounds 1023.95 → "1024.0"; promote to the next unit instead so the
	// printed form stays ≤ "999.9 XiB" (9 cells) for every unit below PiB.
	if idx < len(units)-1 && val+0.05 >= unit {
		val /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", val, units[idx])
}

// FormatFileSizeShort is like FormatFileSize but uses single-letter suffixes
// (K, M, G, T, P) so values stay compact enough for two-part displays like
// "42.0M/256.0M". Max output: "999.9P" (6 chars).
func FormatFileSizeShort(b int64) string {
	const unit = 1024.0
	if b < 0 {
		return unknownGlyph
	}
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	val := float64(b)
	units := []string{"K", "M", "G", "T", "P"}
	idx := -1
	for val >= unit && idx < len(units)-1 {
		val /= unit
		idx++
	}
	if idx < len(units)-1 && val+0.05 >= unit {
		val /= unit
		idx++
	}
	return fmt.Sprintf("%.1f%s", val, units[idx])
}

// FormatSpeed humanises bytes/sec. Always fits in colSpeed once padded
// ("999.9 MiB/s" = 11 cells; an unknown speed shows the shared unknownGlyph).
func FormatSpeed(bps int64) string {
	if bps < 0 {
		return unknownGlyph
	}
	return FormatFileSize(bps) + "/s"
}

// FormatDuration turns a time.Duration into fixed-width HH:MM:SS (the bar's
// <ETA>, always exactly 8 cells for HH:MM:SS format). A nonsensically large
// ETA — produced by estimateETA when the rolling speed has decayed near zero
// mid-stream — is rendered as the shared unknownGlyph (right-aligned into the
// ETA column) rather than overflowing. %7s works because unknownGlyph is a
// single ASCII char and fmt pads by runes, not cells.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return fmt.Sprintf("%7s", unknownGlyph)
	}
	s := int(d.Seconds())
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	if h > 99 {
		return "99:59:59"
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
}

// statusGlyph returns the one-cell state symbol and its colour for a task.
// The glyph gives the state at a glance without colour; the colour reinforces
// it on terminals that support ANSI.
func statusGlyph(v download.ProgressView) (string, Color) {
	switch v.State {
	case download.StateActive:
		return "↓", colorYellow // ↓ downloading
	case download.StateRetrying:
		return "↻", colorRed // ↻ retrying
	case download.StateError:
		return "✗", colorRed // ✗ failed
	case download.StateCompleted:
		return "✓", colorGreen // ✓ done
	case download.StatePaused:
		return "⏸", colorGrey // ⏸ paused
	case download.StateQueued:
		return "…", colorGrey // … waiting
	}
	return " ", ""
}

// RenderSummary formats the bottom summary line (PRD §8.1 example):
//
//	Total: X/Y completed  <bytes>  <speed>  ETA <eta>  +<elapsed>  [bar]  ZZ%
//
// elapsed ≤ 0 (no clock started, non-TTY) omits the elapsed segment. The bar
// is the full BarWidth (non-TTY output has no width constraint).
func RenderSummary(completed, total int, speedBps int64, eta, elapsed time.Duration, bytesDone, totalSize int64, useColor bool) string {
	return renderSummaryWidth(completed, total, speedBps, eta, elapsed, bytesDone, totalSize, useColor, 0, BarWidth, -1)
}

// RenderSummaryWidth is like RenderSummary but right-aligns the info block to
// the given terminal width, matching the pacman-style task line layout. barWidth
// lets the caller shrink the bar on narrow terminals (see layoutFor). faceTick
// is the wall-clock second counter for the aggregate bar's pacman face; pass a
// negative value for a static face (legacy/non-animated callers).
func RenderSummaryWidth(completed, total int, speedBps int64, eta, elapsed time.Duration, bytesDone, totalSize int64, useColor bool, termWidth int, barWidth int, faceTick int64) string {
	return renderSummaryWidth(completed, total, speedBps, eta, elapsed, bytesDone, totalSize, useColor, termWidth, barWidth, faceTick)
}

func renderSummaryWidth(completed, total int, speedBps int64, eta, elapsed time.Duration, bytesDone, totalSize int64, useColor bool, termWidth int, barWidth int, faceTick int64) string {
	if barWidth < 2 {
		barWidth = 2
	}
	sp := fitWidth(FormatSpeed(speedBps), colSpeed)
	etaStr := fitWidth(FormatDuration(eta), colETA)
	elStr := ""
	if elapsed > 0 {
		elStr = fitWidth("+"+FormatDuration(elapsed), colETA+1)
	}
	pctStr := unknownGlyph
	if totalSize > 0 {
		pct := min(max(int(float64(bytesDone)/float64(totalSize)*100), 0), 100)
		pctStr = fmt.Sprintf("%d%%", pct)
	}
	pctCol := fitWidth(pctStr, colPct)
	// The aggregate bar animates its pacman face in step with the per-file bars
	// (same wall-clock faceTick), so "Total" doesn't sit still while the file
	// lines' pacmen pulse.
	bar := BarIndeterminate(bytesDone, totalSize, barWidth, -1, faceTick, "")
	if useColor {
		bar = colorizeBar(bar, true)
	}

	leftText := fmt.Sprintf("Total: %d/%d completed", completed, total)
	bytesStr := FormatFileSize(bytesDone)
	if totalSize > 0 {
		bytesStr += "/" + FormatFileSize(totalSize)
	}

	pctColored := pctCol
	leftColored := leftText
	if useColor {
		pctColored = string(colorGreen) + pctCol + string(colorReset)
		leftColored = string(colorGreen) + leftText + string(colorReset)
	}

	// Full right block (right-aligned on a TTY): <speed>  ETA <eta>  +<elapsed>
	// [bar]  <pct>. The aggregate bytes are deliberately NOT here — with the
	// 30-cell bar, speed, ETA and elapsed the line would exceed 120 columns;
	// the bytes move to the compact tier, and the per-file lines already carry
	// each file's done/total. Colours: speed yellow (live), ETA/elapsed grey
	// (secondary), bar its pacman colours, percent green (done).
	right := fmt.Sprintf("%s  ETA %s", sp, etaStr)
	if elStr != "" {
		right += "  " + elStr
	}
	right += fmt.Sprintf("  [%s]  %s", bar, pctColored)
	if useColor {
		right = strings.Replace(right, sp, string(colorYellow)+sp+string(colorReset), 1)
		right = strings.Replace(right, elStr, string(colorCyan)+elStr+string(colorReset), 1)
	}

	if termWidth > 0 {
		pad := termWidth - displayWidth(leftColored) - displayWidth(right) - 1
		if pad >= 2 {
			return leftColored + strings.Repeat(" ", pad) + right
		}
		// Compact: left + bytes + bar + pct.
		compact := fmt.Sprintf("%s  %s  [%s]  %s", leftColored, bytesStr, bar, pctColored)
		if displayWidth(compact) <= termWidth {
			return compact
		}
		// Minimal: short left ("Total: 3/5", no "completed") + pct.
		leftShort := fmt.Sprintf("Total: %d/%d", completed, total)
		if useColor {
			leftShort = string(colorGreen) + leftShort + string(colorReset)
		}
		min := fmt.Sprintf("%s  %s", leftShort, pctColored)
		if displayWidth(min) <= termWidth {
			return min
		}
		// Floor: the percent alone — always fits any width ≥ 4.
		return pctColored
	}

	// Fixed-width fallback (non-TTY, tests)
	if useColor {
		return fmt.Sprintf("%s  |  %s%s%s  |  ETA %s  %s  [%s]  %s",
			leftColored, string(colorYellow), sp, string(colorReset), etaStr, elStr, bar, pctColored)
	}
	return fmt.Sprintf("%s  |  %s  |  ETA %s  %s  [%s]  %s", leftText, sp, etaStr, elStr, bar, pctCol)
}
