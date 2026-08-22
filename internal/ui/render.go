// SPDX-License-Identifier: MIT
//
// render.go holds the per-file task-line layout: the fixed column widths, the
// state glyph/colour mapping is in summary.go, the pacman bar in bar.go, the
// ANSI-aware string primitives in ansi.go. Everything here is pure — the
// Renderer (progress.go) drives it with per-frame inputs.
//
// Layout (per-file line, widest tier):
//
//	<glyph> <file_name>  <size>  <speed>  <ETA>  [x<N> [bar]]  <pct>% [e<N>]
//
// The glyph column is a one-cell ASCII status symbol (> active, ! retrying,
// x error, + completed, | paused, . queued) so a glance tells the state even
// without colour — ASCII on purpose: ambiguous-width Unicode glyphs render
// two cells wide on some terminals and break the row-fit contract (see
// statusGlyph). The [x<N>] connection count sits inside the bar brackets, pacman
// style. An error count badge (e<N>) appears after the percent only when the
// task actually errored, so the common line never grows. The sizeless
// (indeterminate) bar animates a position-bounce across frames instead of
// parking pacman dead-centre; the indeterminate position is passed in by Bar's
// caller (the Renderer owns the wall-clock tick), so Bar itself stays pure.
//
// Rendering is layered for narrow terminals — full columns, then speed/ETA
// dropped, then the bar dropped, then name+pct, down to a pct-only floor so
// even a 10-column terminal gets a usable line. See layoutFor in progress.go.
package ui

import (
	"fmt"
	"strings"

	"odm/internal/download"
)

// Fixed column widths for renderTaskLine. fmt's %Ns is a *minimum* width and
// grows with the string, so speed ("1.0 KiB/s" → "1023.0 MiB/s") and ETA
// ("00:05" → "2160:00") used to shove the pacman bar and trailing percent
// around every frame. Values are fitted to these widths (right-aligned) so
// the bar stays glued to the right edge of the line.
const (
	// colGlyph is the status-symbol column: one glyph + a space.
	colGlyph = 2
	colSize  = 14 // "999.9M/999.9M" (hybrid done/total)
	colSpeed = 11 // "999.9 MiB/s", "1023 B/s"
	colETA   = 8  // "HH:MM:SS"

	// colConns is a fixed-width slot for the connection indicator "[xN] " so
	// the bar bracket stays aligned regardless of connection count.
	colConns = 6 // "[x32] " max = 6

	// colPct is the trailing percent slot ("  24%" / "   ?").
	colPct = 4
)

// unknownGlyph is the single marker for "value not known yet" in every
// column: sizes, speed, ETA and the percent of a sizeless stream. One glyph
// everywhere so a glance reads the same "not available" across the line.
const unknownGlyph = "?"

// lineLayout picks which column set a per-file line renders, from the
// terminal width (see layoutFor). Narrower screens shed whole columns so the
// line always fits: full → no speed/ETA → no size/conn → name+pct → pct only.
type lineLayout int

const (
	layoutFull       lineLayout = iota // glyph name size speed eta conn bar pct
	layoutNoSpeedETA                   // glyph name size conn bar pct
	layoutNameBarPct                   // glyph name bar pct
	layoutNamePct                      // glyph name pct (barless)
	layoutPct                          // pct only (super-narrow floor)
)

// renderTaskLine formats one per-file line per PRD §8.1, colouring it when
// useColor is true. The full line layout:
//
//	<glyph> <file_name>   <size>   <speed>   <ETA>   [x<N> [<bar>]]   <pct>%
//
// The glyph is a one-cell status symbol (see statusGlyph). Connection count
// is shown as a prefix inside the bar brackets (e.g. [x4---c  o  o]), matching
// the pacman style where progress info sits on the right side of the line.
// A task that errored appends an " e<N>" badge after the percent — the only
// variable-width tail, and it appears only on error, so the common line's
// columns never shift. A sizeless stream shows "?" for the percent instead of
// a misleading 0%.
// indeterminatePos drives the sizeless bar animation; pass -1 for a static
// (centred) indeterminate bar. The pacman face size is position-driven (big
// 'C' on a dot cell, small 'c' between dots — see barLine), so no clock input
// is needed here.
//
// The other layouts shed columns for narrow terminals (see layoutFor); the
// colouring is applied per layout (no bracket → no bracket colouring).
func renderTaskLine(v download.ProgressView, useColor bool, indeterminatePos int, nameWidth int, barWidth int, layout lineLayout) string {
	if nameWidth < 4 && layout != layoutPct {
		nameWidth = 4
	}
	if barWidth < 2 && (layout == layoutFull || layout == layoutNoSpeedETA || layout == layoutNameBarPct) {
		barWidth = 2
	}

	// Status glyph column (always 2 cells: glyph + space).
	glyph, glyphCol := statusGlyph(v)
	glyphCell := glyph + " "
	if useColor && glyphCol != "" {
		glyphCell = string(glyphCol) + glyph + " " + string(colorReset)
	}

	name := v.Filename
	if name == "" {
		name = v.URL
	}
	name = truncateNameTo(name, nameWidth)
	// State-coloured name cell: the glyph column already carries its own
	// colour+reset, so wrapping the whole line would let the glyph's reset
	// kill the name's colour — colour the pieces individually instead.
	nameCell := padToCells(name, nameWidth)
	if c := stateColor(v.State, useColor); c != "" {
		nameCell = string(c) + nameCell + string(colorReset)
	}

	size := FormatFileSizeShort(v.BytesDone)
	if v.TotalSize > 0 {
		size += "/" + FormatFileSizeShort(v.TotalSize)
	} else if v.TotalSize < 0 {
		size += "/" + unknownGlyph
	}
	// For completed tasks show final size (pinned, not "0/?" for sizeless).
	if v.TotalSize <= 0 && v.State == download.StateCompleted && v.BytesDone > 0 {
		size = FormatFileSizeShort(v.BytesDone) + "/" + FormatFileSizeShort(v.BytesDone)
	}
	speed := FormatSpeed(v.Speed)
	eta := FormatDuration(v.ETA)

	// Fixed-width connection indicator "[xN]  " before the bar bracket.
	connDisplay := strings.Repeat(" ", colConns)
	if v.Connections > 0 && v.State != download.StateCompleted {
		connStr := fmt.Sprintf("[x%d]", v.Connections)
		connDisplay = connStr + strings.Repeat(" ", colConns-len(connStr))
	}

	bar := BarIndeterminate(v.BytesDone, v.TotalSize, barWidth, indeterminatePos, "")
	// Honest percent: "?" for a sizeless stream (no total to measure against),
	// otherwise the rounded 0..100 with the shared unknownGlyph for weirdness.
	pctStr := unknownGlyph
	if v.TotalSize > 0 {
		pct := min(max(int(float64(v.BytesDone)/float64(v.TotalSize)*100), 0), 100)
		pctStr = fmt.Sprintf("%d%%", pct)
	}
	pctCol := fitWidth(pctStr, colPct)

	// Fixed columns: glyph | name | size | speed | ETA | <conn> [<bar>] | pct
	// Speed/ETA/size are fitted so a slow→fast or short→long ETA transition
	// cannot walk the pacman bar and the right-edge percent left/right. The
	// name is padded by display cells (padToCells), not runes, so a wide CJK
	// filename can't push the info block past the terminal edge.
	var line string
	switch layout {
	case layoutPct:
		line = fitWidth(pctStr, nameWidth)
	case layoutNoSpeedETA:
		line = fmt.Sprintf("%s%s  %s  %s[%s]  %s",
			glyphCell, nameCell,
			fitWidth(size, colSize),
			connDisplay, bar, pctCol)
	case layoutNameBarPct:
		line = fmt.Sprintf("%s%s  %s[%s]  %s",
			glyphCell, nameCell,
			connDisplay, bar, pctCol)
	case layoutNamePct:
		line = fmt.Sprintf("%s%s  %s", glyphCell, nameCell, pctCol)
	default: // layoutFull
		line = fmt.Sprintf("%s%s  %s  %s  %s  %s[%s]  %s",
			glyphCell, nameCell,
			fitWidth(size, colSize),
			fitWidth(speed, colSpeed),
			fitWidth(eta, colETA),
			connDisplay, bar, pctCol)
	}

	if useColor {
		// Component colors on top of the state-colored line.
		//
		// Percent: state-colored (yellow active, green completed, red error,
		// grey paused/queued). The glyph column already carries its own colour
		// for paused/queued; the percent mirroring it keeps the whole line's
		// colour story per state.
		switch v.State {
		case download.StateActive, download.StateRetrying:
			line = colorReplace(line, pctCol, string(colorYellow)+pctCol+string(colorReset))
		case download.StateCompleted:
			line = colorReplace(line, pctCol, string(colorGreen)+pctCol+string(colorReset))
		case download.StateError:
			line = colorReplace(line, pctCol, string(colorRed)+pctCol+string(colorReset))
		case download.StatePaused, download.StateQueued:
			line = colorReplace(line, pctCol, string(colorGrey)+pctCol+string(colorReset))
		}
		// Pacman bar (face→yellow, dashes→green, dots→cyan). The barless
		// layouts have none.
		if layout == layoutFull || layout == layoutNoSpeedETA || layout == layoutNameBarPct {
			line = colorizeBarInLine(line)
		}
	}

	// Error badge — the only trailing, variable-width element. It appears only
	// when the task actually errored, so the common line never shifts; on very
	// narrow screens the tail is truncated by the caller, never the columns.
	if v.Errors > 0 && (layout == layoutFull || layout == layoutNoSpeedETA || layout == layoutNameBarPct) {
		badge := fmt.Sprintf(" e%d", v.Errors)
		if useColor {
			badge = string(colorRed) + badge + string(colorReset)
		}
		line += badge
	}
	return line
}

// colorReplace replaces the first occurrence of old in s with new.
func colorReplace(s, old, new string) string {
	return strings.Replace(s, old, new, 1)
}

// colorizeBarInLine finds the bar portion inside [...] brackets in a rendered
// line and applies color to its face/dash/dot characters. This is a post-pass
// so the column padding remains correct (ANSI codes don't affect fitWidth).
func colorizeBarInLine(line string) string {
	// Find the last '[' before a ']' — the bar bracket pair.
	open := strings.LastIndex(line, "[")
	if open < 0 {
		return line
	}
	close := strings.Index(line[open:], "]")
	if close < 0 {
		return line
	}
	close += open
	bar := line[open+1 : close]
	return line[:open+1] + colorizeBar(bar, true) + line[close:]
}
