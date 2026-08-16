// SPDX-License-Identifier: MIT
//
// color.go owns the ANSI colour constants and the state→colour mapping shared
// across the renderer (task lines, summary, glyphs, bars).
package ui

import "odm/internal/download"

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
