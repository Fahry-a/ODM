// SPDX-License-Identifier: MIT
//
// termsize.go exposes the terminal width the live redraw needs to keep one
// logical line == one physical line (PRD §8 bug §3.3): if a rendered line is
// wider than the terminal it wraps, the ANSI cursor-up count (stored per
// frame) no longer matches the number of physical rows actually on screen and
// subsequent redraws corrupt the display — doubly so for long filenames in a
// narrow terminal.
//
// We deliberately do NOT import golang.org/x/term: it is not a known dependency
// of this module and adding it would pull a vanity import that the project's
// sandbox/CI egress often can't resolve (per the rewrite spec §6). The standard
// syscall(TIOCGWINSZ) ioctl is all the renderer needs, on a Linux build tag.

package ui

import (
	"io"
	"os"
)

// MinTerminalWidth is the floor used when width detection fails or the writer
// is not a terminal. 80 matches the conventional default column count.
const MinTerminalWidth = 80

// terminalColumns returns the number of columns of the terminal attached to w.
// It returns (cols, ok) where ok is false when w is not a TTY or the ioctl
// fails — callers fall back to MinTerminalWidth in that case.
//
// The renderer truncates every rendered line to cols-1 (see RenderTaskLine's
// caller in Frame) so a line never wraps, which is what keeps the cursor-up
// tally correct frame to frame.
func terminalColumns(w io.Writer) (cols int, ok bool) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, false
	}
	return ioctlWinSize(f.Fd())
}

// rendererWidth folds terminalColumns into a single usable number: the live tty
// width minus 1 (room so a full-width line won't wrap), or MinTerminalWidth-1
// when the writer isn't a terminal. Re-read every frame so a mid-run terminal
// resize is picked up — the call is a single cheap ioctl.
func rendererWidth(w io.Writer) int {
	if c, ok := terminalColumns(w); ok && c > 0 {
		return c - 1
	}
	return MinTerminalWidth - 1
}
