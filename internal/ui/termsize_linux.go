//go:build linux

package ui

import (
	"syscall"
	"unsafe"
)

// winsize mirrors struct winsize from <termios.h>. The kernel fills col/row
// for the tty whose fd we pass to the TIOCGWINSZ ioctl.
type winsize struct {
	row    uint16
	col    uint16
	xpixel uint16
	ypixel uint16
}

// ioctlWinSize asks the kernel for the tty size behind fd via TIOCGWINSZ.
// Returns (cols, rows, true) on success. A non-tty fd or a failing ioctl
// yields (0, 0, false); the caller falls back to the conventional 80×24
// default.
//
// We use syscall.Syscall6 rather than unix.IoctlGetWinsize so golang.org/x/sys
// (and transitively golang.org/x/term) never enters the dependency graph.
func ioctlWinSize(fd uintptr) (cols, rows int, ok bool) {
	ws := winsize{}
	// SYS_IOCTL + TIOCGWINSZ is the same across Linux architectures.
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
		0, 0, 0,
	)
	if errno != 0 || ws.col == 0 {
		return 0, 0, false
	}
	return int(ws.col), int(ws.row), true
}
