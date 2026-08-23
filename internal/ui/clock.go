// SPDX-License-Identifier: MIT
//
// clock.go is a tiny seam around time so the Renderer's two time-driven
// behaviours — the indeterminate (sizeless) bar animation and the non-TTY log
// throttle — are deterministic in tests. The default clock is the wall clock;
// tests inject a fakeClock whose Now() they advance by hand.

package ui

import "time"

// nowFn is the clock the Renderer reads. Defaults to time.Now; tests replace it
// via the package-level setClock seam (see clock_test in progress_test.go).
// Keeping it a package var rather than a field lets the pure helpers in
// render.go (BarIndeterminate via Frame) read the same clock without threading
// it through every signature — they don't use it; only Frame does.
var nowFn = func() time.Time { return time.Now() }

// bouncePosition maps a frame index to a back-and-forth slot in [0,width) so
// the sizeless pacman travels left→right→left over the bar instead of sitting
// still (bug). The motion is a triangle wave: slot rises 0→width-1 across
// the first `width` frames, then falls back width-2→1 over the next width-2
// frames, so neither endpoint is visited twice; the period is
// 2*(width-1) frames (and 1 for a single-cell bar, where the slot is pinned).
// width must be ≥1; the result is clamped into [0,width-1].
func bouncePosition(frame int, width int) int {
	if width <= 1 {
		return 0
	}
	period := 2 * (width - 1)
	stp := frame % period
	if stp < 0 {
		stp += period
	}
	if stp >= width {
		stp = period - stp // descending half: width-2, width-3, …, 1
	}
	if stp < 0 {
		stp = 0
	}
	if stp >= width {
		stp = width - 1
	}
	return stp
}
