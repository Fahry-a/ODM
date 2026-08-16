// SPDX-License-Identifier: MIT
//
// ansi.go owns the ANSI-aware string primitives the renderer builds on:
// terminal-cell width accounting (wide runes count double), rune-safe
// truncation that preserves escape sequences, and the single ANSI scanner
// shared by both. Keeping the ANSI grammar in one place means width math and
// truncation can never desync (two independent parsers were two places for
// one desync).
package ui

import (
	"strings"
	"unicode/utf8"
)

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
