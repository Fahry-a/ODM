// SPDX-License-Identifier: MIT
//
// termsize.go exposes the terminal size the live redraw needs to keep one
// logical line == one physical line: if a rendered line is
// wider than the terminal it wraps, the ANSI cursor-up count (stored per
// frame) no longer matches the number of physical rows actually on screen and
// subsequent redraws corrupt the display — doubly so for long filenames in a
// narrow terminal.
//
// The height (rows) is read too: a batch taller than the terminal would push
// the cursor-up count past the top of the screen on the next redraw, corrupting
// the display the same way wrapping does. Frame() caps the rendered task list
// to the row budget.
//
// We deliberately do NOT import golang.org/x/term: it is not a known dependency
// of this module and adding it would pull a vanity import that the project's
// sandbox/CI egress often can't resolve (per the rewrite decision). The standard
// syscall(TIOCGWINSZ) ioctl is all the renderer needs, on a Linux build tag.

package ui

import (
	"io"
	"os"
)

// MinTerminalWidth is the floor used when width detection fails or the writer
// is not a terminal. 80 matches the conventional default column count.
const MinTerminalWidth = 80

// MinTerminalHeight is the floor used when height detection fails or the
// writer is not a terminal. 24 matches the conventional default row count.
const MinTerminalHeight = 24

// terminalSize returns the size of the terminal attached to w. It returns
// (cols, rows, ok) where ok is false when w is not a TTY or the ioctl fails —
// callers fall back to the Min* defaults in that case.
func terminalSize(w io.Writer) (cols, rows int, ok bool) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, 0, false
	}
	return ioctlWinSize(f.Fd())
}

// rendererSize folds terminalSize into usable numbers: the live tty size
// minus 1 column (room so a full-width line won't wrap), or the Min*
// defaults when the writer isn't a terminal. Re-read every frame so a mid-run
// terminal resize is picked up — the call is a single cheap ioctl.
func rendererSize(w io.Writer) (width, height int) {
	if c, r, ok := terminalSize(w); ok && c > 0 {
		return c - 1, r
	}
	return MinTerminalWidth - 1, MinTerminalHeight
}
