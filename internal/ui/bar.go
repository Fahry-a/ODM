// SPDX-License-Identifier: MIT
//
// bar.go owns the pacman/ILoveCandy progress bar: the fixed-width bar
// rendering (Bar/BarIndeterminate/barLine) and its ANSI colouring
// (colorizeBar). Rendering is deliberately pure — the caller (Renderer) owns
// the wall-clock tick for the indeterminate bounce and the face animation, so
// every function here is deterministic given its inputs and stays testable.
package ui

import (
	"strings"
)

// BarWidth is the visual width of the pacman bar (in cells) on wide terminals.
const BarWidth = 30

// pacFace is the pacman icon — the "c" from ILoveCandy in the original Arch
// animation; rendered as "c" per the PRD §8 spec text.
const pacFace = "c"

// pacFaceAlt is the alternate pacman face for the expand/shrink animation
// (uppercase 'C' every second).
const pacFaceAlt = "C"

// pacFaceInterval is the wall-clock interval (in seconds) each face state
// lasts, so the expand/shrink cycle (c→C→c) takes 2×faceInterval —
// "setiap detik huruf c berubah besar lalu kecil". The face animation is
// TIME-driven, not frame-driven: tying it to the render frame counter made it
// speed up when frames arrive faster than the ~100ms cadence (a SIGWINCH
// resize storm, an event burst), so the face would flicker gede-kecil rapidly.
// faceTick below is wall-clock seconds, so the cycle stays ~1s per state no
// matter how many frames render in between.
const pacFaceInterval = 1

// Bar renders one pacman progress bar into a fixed-width string. `done`/`total`
// are done/total bytes; we compute the fraction eaten and draw:
//
//	eaten region  → "-" (blank/dashes)
//	pacman face   → "c" or "C" (animates every second)
//	remaining     → "o o o …" (dots with spaces between)
//
// The bar is always exactly `width` display cells: dashes and face take 1 cell
// each; remaining dots take 2 cells each ("o " pair) with the trailing space
// trimmed, and the line is padded with a trailing space when needed so the bar
// never grows wider than `width` (which would shove the percent column).
//
// At 100% the whole bar is dashes/blank (PRD §8 "nothing left for pacman to
// eat"). The extras over the previous build:
//   - the indeterminate (total<=0) bar animates: pos is the bounce position
//     [0..width) the Renderer advances per frame (see tickIndeterminate). When
//     pos<0 the bar falls back to the static centre layout Bar() stored before
//     (kept for callers/tests that don't drive the tick).
func Bar(done, total int64, width int) string {
	return BarIndeterminate(done, total, width, -1, 0, "")
}

// BarIndeterminate is Bar with an explicit pacman position for the sizeless
// (indeterminate) case. pos==-1 means "no animation slot provided" and the
// static centred layout is used (back-compat with the pure Bar() contract the
// tests pin). pos is clamped to [0,width-1]. faceTick is the wall-clock time
// in seconds (see faceIntervalFor); a negative value disables the face
// animation (static 'c'), which is what the summary's Bar() uses. prefix is
// rendered before the bar content (e.g. "x4 " for connection count) and is
// included in the width budget.
func BarIndeterminate(done, total int64, width int, pos int, faceTick int64, prefix string) string {
	if width < 2 {
		width = 2
	}

	// Determine which pacman face to show (animates every pacFaceInterval
	// seconds of wall-clock time, independent of render cadence).
	face := pacFace
	if faceTick >= 0 {
		cycle := (faceTick / pacFaceInterval) % 2
		if cycle == 1 {
			face = pacFaceAlt
		}
	}

	if total <= 0 {
		// Sizeless stream: indeterminate. If the caller passed a live bounce
		// position we honour it; otherwise pacman parks at the middle.
		if pos < 0 {
			half := width / 2
			return barLine(half, face, width, prefix)
		}
		if pos >= width-1 {
			pos = width - 1
		}
		if pos < 0 {
			pos = 0
		}
		return barLine(pos, face, width, prefix)
	}
	if done >= total {
		return strings.Repeat("-", width) // fully eaten
	}
	frac := float64(done) / float64(total)
	eaten := int(frac * float64(width))
	if eaten >= width {
		eaten = width - 1
	}
	return barLine(eaten, face, width, prefix)
}

// barLine renders: prefix + `eaten` dashes + face + remaining fill from a
// fixed alternating dot pattern (first position after face is always 'o',
// then space, then 'o', etc.), padded to exactly `width` display cells.
// Unlike the previous re-spaced approach (which recomputed spacedDots from
// the remaining width every frame, causing dots to shift right), this keeps
// the same alternating sequence anchored to the face position so dots never
// jump — they shift at most 1 cell per frame as the face advances.
func barLine(eaten int, face string, width int, prefix string) string {
	pLen := len([]rune(prefix))
	avail := width - pLen
	if avail < 1 {
		return prefix + strings.Repeat("-", eaten) + face
	}

	if eaten >= avail {
		return prefix + strings.Repeat("-", avail)
	}

	var b strings.Builder
	b.Grow(width)
	b.WriteString(prefix)

	for i := 0; i < eaten; i++ {
		b.WriteByte('-')
	}
	b.WriteString(face)

	// Fill remaining cells with a fixed alternating pattern anchored
	// to absolute position, so dots stay in place and don't shift right
	// as pacman advances through them.
	for pos := eaten + 1; pos < avail; pos++ {
		if pos%2 == 0 {
			b.WriteByte('o')
		} else {
			b.WriteByte(' ')
		}
	}

	s := b.String()
	for len([]rune(s)) < width {
		s += " "
	}
	return s
}

// colorizeBar applies ANSI colors to the pacman bar characters when useColor
// is true: face (c/C) → yellow, dashes (eaten) → green, dots (remaining) → cyan.
// When useColor is false, the bar is returned unchanged.
func colorizeBar(bar string, useColor bool) string {
	if !useColor {
		return bar
	}
	var b strings.Builder
	b.Grow(len(bar) + 20)
	for _, r := range bar {
		switch r {
		case 'c', 'C':
			b.WriteString(string(colorYellow))
			b.WriteRune(r)
			b.WriteString(string(colorReset))
		case '-':
			b.WriteString(string(colorGreen))
			b.WriteRune(r)
			b.WriteString(string(colorReset))
		case 'o':
			b.WriteString(string(colorCyan))
			b.WriteRune(r)
			b.WriteString(string(colorReset))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// minBarWidth is the floor for the adaptive progress bar on narrow terminals.
// Below the full-layout width the bar shrinks (to this floor), then whole
// columns drop; see layoutFor.
const minBarWidth = 10

// infoFixedWidth is the display width of everything after the name field for
// the FULL layout that does NOT depend on the bar width:
// "  <size>  <speed>  <ETA>  <conn> [ ]  <pct>".
const infoFixedWidth = 2 + colSize + 2 + colSpeed + 2 + colETA + 2 + colConns + 1 + 1 + 2 + colPct

// infoBlockWidthFor is the full display width of the info block (everything
// after the name field) for a given bar width, including the status-glyph
// column that precedes the name.
func infoBlockWidthFor(barWidth int) int { return colGlyph + infoFixedWidth + barWidth }
