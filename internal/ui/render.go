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

// pacFaceAlt is the alternate pacman face for the expand/shrink animation
// (uppercase 'C' every second).
const pacFaceAlt = "C"

// pacFaceFrameDuration is how many render frames (at ~100ms each) each face
// state lasts. 5 frames = ~500ms per state, so the full expand/shrink cycle
// (c→C→c) takes ~1 second — "setiap detik huruf c berubah besar lalu kecil".
const pacFaceFrameDuration = 5

// Color is a state.colour pair (foreground). "" → no colour (non-TTY).
type Color string

const (
	colorReset   Color = "\x1b[0m"
	colorGreen   Color = "\x1b[32m"
	colorYellow  Color = "\x1b[33m"
	colorRed     Color = "\x1b[31m"
	colorGrey    Color = "\x1b[90m"
	colorCyan    Color = "\x1b[36m"
	colorMagenta Color = "\x1b[35m"
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

// Fixed column widths for RenderTaskLine. fmt's %Ns is a *minimum* width and
// grows with the string, so speed ("1.0 KiB/s" → "1023.0 MiB/s") and ETA
// ("00:05" → "2160:00") used to shove the pacman bar and trailing percent
// around every frame. Values are fitted to these widths (right-aligned) so
// the bar stays glued to the right edge of the line.
const (
	colSize  = 14 // "999.9M/999.9M" (hybrid done/total)
	colSpeed = 11 // "999.9 MiB/s", "1023 B/s"
	colETA   = 8  // "HH:MM:SS" (was MM:SS = 5)

	// colConns is a fixed-width slot for the connection indicator "[xN] " so
	// the bar bracket stays aligned regardless of connection count.
	colConns = 6 // "[x32] " max = 6
)

// unknownGlyph is the single marker for "value not known yet" in every
// column: sizes, speed and ETA. One glyph everywhere so a glance reads the
// same "not available" across the line (was "?" vs "--" vs "--:--:--").
const unknownGlyph = "?"

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

// runeWidth returns the number of terminal cells r occupies: East Asian wide
// and fullwidth characters (CJK ideographs, Hangul, Kana, fullwidth forms,
// emoji/pictographs, CJK Ext B+) take two cells, everything else one. This
// mirrors wcwidth's East Asian Wide ranges without pulling in a dependency.
func runeWidth(r rune) int {
	if r >= 0x1100 &&
		(r <= 0x115f || // Hangul Jamo
			r == 0x2329 || r == 0x232a || // angle brackets
			(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) || // CJK .. Yi
			(r >= 0xac00 && r <= 0xd7a3) || // Hangul Syllables
			(r >= 0xf900 && r <= 0xfaff) || // CJK Compatibility Ideographs
			(r >= 0xfe10 && r <= 0xfe19) || // Vertical forms
			(r >= 0xfe30 && r <= 0xfe6f) || // CJK Compatibility Forms
			(r >= 0xff00 && r <= 0xff60) || // Fullwidth forms
			(r >= 0xffe0 && r <= 0xffe6) || // Fullwidth signs
			(r >= 0x1f300 && r <= 0x1faff) || // emoji / pictographs
			(r >= 0x1f900 && r <= 0x1f9ff) || // Supplemental Symbols
			(r >= 0x20000 && r <= 0x2fffd) || // CJK Ext B+
			(r >= 0x30000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

// displayWidth returns the number of terminal cells s occupies; wide runes
// count double and ANSI escape sequences contribute zero width.
func displayWidth(s string) int {
	w := 0
	for _, seg := range scanAnsi(s) {
		if seg.visible {
			w += seg.width
		}
	}
	return w
}

// truncateVisibleWidth cuts s so that only the first `width` visible cells are
// kept, preserving all ANSI escape sequences intact. Sequences that span the
// cut point are dropped cleanly (the cut happens on a visible character
// boundary, and the reset code is appended only if an escape sequence was
// active at the cut point).
func truncateVisibleWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	var b strings.Builder
	remaining := width
	wasColorActive := false
	for _, seg := range scanAnsi(s) {
		if !seg.visible {
			b.WriteString(seg.text)
			// Track whether we're inside a non-reset escape sequence.
			wasColorActive = !strings.Contains(seg.text, string(colorReset))
			continue
		}
		if remaining <= 0 {
			break
		}
		b.WriteString(seg.text)
		remaining -= seg.width
		wasColorActive = false
	}
	// Append a reset only if we truncated while color was active.
	if remaining <= 0 && wasColorActive {
		b.WriteString(string(colorReset))
	}
	return b.String()
}

// ansiSeg is one span of s: either an ANSI escape sequence (visible=false) or
// a single visible rune (visible=true, width = its terminal cells).
type ansiSeg struct {
	text    string
	visible bool
	width   int
}

// scanAnsi splits s into visible runes and ANSI escape sequences. A malformed
// or partial sequence (no terminating final byte) falls through and is treated
// as visible text, so an unterminated escape can never swallow characters.
//
// Sharing one scanner between displayWidth and truncateVisibleWidth keeps the
// ANSI grammar in a single place — two independent parsers were two places for
// a desync (one skipping a sequence the other counts).
func scanAnsi(s string) []ansiSeg {
	segs := make([]ansiSeg, 0, len(s)/4+1)
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
				segs = append(segs, ansiSeg{text: s[i : j+1]})
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			segs = append(segs, ansiSeg{text: s[i : i+1], visible: true, width: 1})
			i++
			continue
		}
		segs = append(segs, ansiSeg{text: s[i : i+size], visible: true, width: runeWidth(r)})
		i += size
	}
	return segs
}

// fitWidth right-aligns s into exactly w display cells. Wide runes (CJK/emoji)
// count as 2 cells; a longer string is truncated so a pathological value still
// cannot shove neighbouring columns.
func fitWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	d := displayWidth(s)
	if d == w {
		return s
	}
	if d > w {
		return truncateVisibleWidth(s, w)
	}
	return strings.Repeat(" ", w-d) + s
}

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
// tests pin). pos is clamped to [0,width-1]. frame is the global frame counter
// used to animate the pacman face (c/C) every ~1 second. prefix is rendered
// before the bar content (e.g. "x4 " for connection count) and is included in
// the width budget.
func BarIndeterminate(done, total int64, width int, pos int, frame int, prefix string) string {
	if width < 2 {
		width = 2
	}

	// Determine which pacman face to show (animates every ~1 second)
	face := pacFace
	if frame >= 0 {
		cycle := (frame / pacFaceFrameDuration) % 2
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
// The fixed 30-cell bar plus the info block needs ~96 columns; below that the
// bar shrinks (to this floor) so the percent column stays on screen instead of
// being truncated off the right edge.
const minBarWidth = 10

// infoFixedWidth is the display width of everything after the name field that
// does NOT depend on the bar width: "  <size>  <speed>  <ETA>  <conn> [ ]  <pct>%".
// The bar's own width is added by infoBlockWidthFor.
const infoFixedWidth = 2 + colSize + 2 + colSpeed + 2 + colETA + 2 + colConns + 1 + 1 + 2 + 4

// infoBlockWidthFor is the full display width of the info block (everything
// after the name field) for a given bar width.
func infoBlockWidthFor(barWidth int) int { return infoFixedWidth + barWidth }

// barWidthFor picks a bar width that fits termWidth (the renderer width, i.e.
// terminal columns minus 1) while still leaving ≥10 display cells for the
// filename, clamped to [minBarWidth, BarWidth]. On wide terminals the full
// bar is used; on narrow ones the bar — and only the bar — shrinks.
func barWidthFor(termWidth int) int {
	bw := termWidth - infoFixedWidth - 1 - 10
	if bw < minBarWidth {
		bw = minBarWidth
	}
	if bw > BarWidth {
		bw = BarWidth
	}
	return bw
}

// truncateNameTo cuts name to the given max display cells, rune-safe and wide-
// aware: CJK/emoji count 2 cells, and the ellipsis reserves 3 cells.
func truncateNameTo(name string, max int) string {
	if max <= 0 || displayWidth(name) <= max {
		return name
	}
	budget := max - 3
	if budget < 0 {
		budget = 0
	}
	var b strings.Builder
	for _, r := range name {
		w := runeWidth(r)
		if budget-w < 0 {
			break
		}
		b.WriteRune(r)
		budget -= w
	}
	return b.String() + "..."
}

// padToCells left-justifies s and pads it with spaces to exactly w display
// cells (wide runes count double). The caller is expected to have truncated
// s to ≤w cells already; this only adds the padding fmt's %-*s can't because
// it pads by runes, not cells.
func padToCells(s string, w int) string {
	if d := displayWidth(s); d < w {
		return s + strings.Repeat(" ", w-d)
	}
	return s
}

// lineLayout picks which column set a per-file line renders, from the
// terminal width (see layoutFor). Narrower screens shed whole columns so the
// line always fits: full → no speed/ETA → no size/conn → name + pct only.
type lineLayout int

const (
	layoutFull       lineLayout = iota // name size speed eta conn bar pct (PRD §8.1)
	layoutNoSpeedETA                   // name size conn bar pct
	layoutNameBarPct                   // name bar pct
	layoutNamePct                      // name pct (barless, very narrow)
)

// RenderTaskLine formats one per-file line per PRD §8.1, colouring it when
// useColor is true. The full line layout:
//
//	<file_name>   <size>   <speed>/s   <ETA>   [<bar>]   <percent>%
//
// Connection count is shown as a prefix inside the bar brackets (e.g. [x4---c  o  o]),
// matching the pacman style where progress info sits on the right side of the line.
// indeterminatePos drives the sizeless bar animation; pass -1 for a static
// (centred) indeterminate bar. frame is the global frame counter for the
// pacman face animation (c/C every ~1s).
//
// The other layouts shed columns for narrow terminals (see layoutFor); the
// colouring is applied per layout (no bracket → no bracket colouring).
func renderTaskLine(v download.ProgressView, useColor bool, indeterminatePos int, frame int, nameWidth int, barWidth int, layout lineLayout) string {
	if nameWidth < 4 {
		nameWidth = 4
	}
	if barWidth < 2 {
		barWidth = 2
	}
	name := v.Filename
	if name == "" {
		name = v.URL
	}
	name = truncateNameTo(name, nameWidth)
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

	// Build a fixed-width connection indicator "[xN]  " in its own bracket
	// before the progress bar, so it doesn't visually merge with bar content.
	connDisplay := strings.Repeat(" ", colConns)
	if v.Connections > 0 && v.State != download.StateCompleted {
		connStr := fmt.Sprintf("[x%d]", v.Connections)
		// Left-align with trailing spaces to fill colConns.
		connDisplay = connStr + strings.Repeat(" ", colConns-len(connStr))
	}

	bar := BarIndeterminate(v.BytesDone, v.TotalSize, barWidth, indeterminatePos, frame, "")
	pct := 0
	if v.TotalSize > 0 {
		pct = min(max(int(float64(v.BytesDone)/float64(v.TotalSize)*100), 0), 100)
	}
	c := stateColor(v.State, useColor)
	reset := ""
	if c != "" {
		reset = string(colorReset)
	}
	// Fixed columns: name | size | speed | ETA | <conn> [<bar>] | pct%
	// Speed/ETA/size are fitted so a slow→fast or short→long ETA transition
	// cannot walk the pacman bar and the right-edge percent left/right. The
	// name is padded by display cells (padToCells), not runes, so a wide CJK
	// filename can't push the info block past the terminal edge.
	var line string
	switch layout {
	case layoutNoSpeedETA:
		line = fmt.Sprintf("%s%s  %s  %s[%s]  %3d%%%s",
			c, padToCells(name, nameWidth),
			fitWidth(size, colSize),
			connDisplay, bar, pct, reset)
	case layoutNameBarPct:
		line = fmt.Sprintf("%s%s  %s[%s]  %3d%%%s",
			c, padToCells(name, nameWidth),
			connDisplay, bar, pct, reset)
	case layoutNamePct:
		line = fmt.Sprintf("%s%s  %3d%%%s", c, padToCells(name, nameWidth), pct, reset)
	default: // layoutFull
		line = fmt.Sprintf("%s%s  %s  %s  %s  %s[%s]  %3d%%%s",
			c, padToCells(name, nameWidth),
			fitWidth(size, colSize),
			fitWidth(speed, colSpeed),
			fitWidth(eta, colETA),
			connDisplay, bar, pct, reset)
	}
	if !useColor {
		return line
	}

	// Apply component-level colors on top of the state-colored line.
	// Color the connection indicator: magenta for active, grey for queued,
	// green for completed (though completed won't show the bracket). Layouts
	// without the conn/bracket columns skip it.
	var connColor Color
	switch v.State {
	case download.StateActive, download.StateRetrying:
		connColor = colorMagenta
	case download.StateQueued, download.StatePaused:
		connColor = colorGrey
	}
	if layout != layoutNamePct && connColor != "" && v.Connections > 0 && v.State != download.StateCompleted {
		connStr := fmt.Sprintf("[x%d]", v.Connections)
		coloredConn := string(connColor) + connStr + string(colorReset)
		connPadded := coloredConn + strings.Repeat(" ", colConns-len(connStr))
		line = strings.Replace(line, connDisplay, connPadded, 1)
	}
	// Color the percentage.
	pctStr := fmt.Sprintf("%3d%%", pct)
	switch v.State {
	case download.StateActive, download.StateRetrying:
		line = colorReplace(line, pctStr, string(colorYellow)+pctStr+string(colorReset))
	case download.StateCompleted:
		line = colorReplace(line, pctStr, string(colorGreen)+pctStr+string(colorReset))
	case download.StateError:
		line = colorReplace(line, pctStr, string(colorRed)+pctStr+string(colorReset))
	}
	// Colorize the pacman bar (face→yellow, dashes→green, dots→cyan). The bar
	// is the bracketed span; the name+pct layout has none.
	if layout != layoutNamePct {
		line = colorizeBarInLine(line)
	}
	return line
}

// colorReplace replaces the first occurrence of old in s with new.
func colorReplace(s, old, new string) string {
	i := strings.Index(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
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

// RenderSummary formats the bottom summary line (PRD §8.1 example):
//
//	Total: X/Y completed  |  <speed>/s  |  ETA HH:MM:SS  [====--]  ZZ%
//
// The bar is the full BarWidth (non-TTY output has no width constraint).
func RenderSummary(completed, total int, speedBps int64, eta time.Duration, bytesDone, totalSize int64, useColor bool) string {
	return renderSummaryWidth(completed, total, speedBps, eta, bytesDone, totalSize, useColor, 0, BarWidth)
}

// RenderSummaryWidth is like RenderSummary but right-aligns the info block to
// the given terminal width, matching the pacman-style task line layout. barWidth
// lets the caller shrink the bar on narrow terminals (see barWidthFor).
func RenderSummaryWidth(completed, total int, speedBps int64, eta time.Duration, bytesDone, totalSize int64, useColor bool, termWidth int, barWidth int) string {
	return renderSummaryWidth(completed, total, speedBps, eta, bytesDone, totalSize, useColor, termWidth, barWidth)
}

func renderSummaryWidth(completed, total int, speedBps int64, eta time.Duration, bytesDone, totalSize int64, useColor bool, termWidth int, barWidth int) string {
	if barWidth < 2 {
		barWidth = 2
	}
	sp := fitWidth(FormatSpeed(speedBps), colSpeed)
	etaStr := fitWidth(FormatDuration(eta), colETA)
	pct := 0
	if totalSize > 0 {
		pct = min(max(int(float64(bytesDone)/float64(totalSize)*100), 0), 100)
	}
	pctStr := fmt.Sprintf("%3d%%", pct)
	bar := Bar(bytesDone, totalSize, barWidth)
	if useColor {
		bar = colorizeBar(bar, true)
	}

	leftText := fmt.Sprintf("Total: %d/%d completed", completed, total)
	rightSide := fmt.Sprintf("  |  %s  |  ETA %s  [%s]  %s", sp, etaStr, bar, pctStr)
	bytesStr := FormatFileSize(bytesDone)
	if totalSize > 0 {
		bytesStr += "/" + FormatFileSize(totalSize)
	}

	if termWidth > 0 {
		padding := termWidth - displayWidth(leftText) - displayWidth(rightSide) - 1
		if padding < 2 {
			// No room for the full summary: drop the speed/ETA right block and
			// emit a one-line summary (left + bytes + bar + pct) sized to fit
			// the terminal — or just left + pct if the bar has no room either.
			// Never truncate the pct off the right edge (the per-file lines
			// already carry the bytes).
			pctColored := pctStr
			if useColor {
				pctColored = string(colorGreen) + pctStr + string(colorReset)
			}
			compact := fmt.Sprintf("%s%s%s %s [%s] %s",
				c0(useColor, colorGreen), leftText, c1(useColor),
				bytesStr, bar, pctColored)
			if displayWidth(compact) <= termWidth {
				return compact
			}
			// Still too wide (extreme narrow widths): drop the byte count too
			// — "Total: N/M" + pct is the absolute floor, always fits.
			return fmt.Sprintf("%s%s%s %s",
				c0(useColor, colorGreen), leftText, c1(useColor), pctColored)
		}
		if useColor {
			return fmt.Sprintf("%s%s%s%s%s",
				string(colorGreen), leftText, string(colorReset),
				strings.Repeat(" ", padding), rightSide)
		}
		return leftText + strings.Repeat(" ", padding) + rightSide
	}

	// Fixed-width fallback (non-TTY, tests)
	if useColor {
		return fmt.Sprintf("%s%s%s  |  %s%s%s  |  ETA %s  [%s]  %s%s%s",
			string(colorGreen), leftText, string(colorReset),
			string(colorYellow), sp, string(colorReset),
			etaStr, bar,
			string(colorGreen), pctStr, string(colorReset))
	}
	return fmt.Sprintf("%s  |  %s  |  ETA %s  [%s]  %s", leftText, sp, etaStr, bar, pctStr)
}

// c0/c1 are tiny colour-pair helpers for the compact summary: the green
// colour code when coloured output is on, or empty strings when not.
func c0(useColor bool, col Color) string {
	if useColor {
		return string(col)
	}
	return ""
}

func c1(useColor bool) string {
	if useColor {
		return string(colorReset)
	}
	return ""
}
