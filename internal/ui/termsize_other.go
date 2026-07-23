//go:build !linux

package ui

// ioctlWinSize is the non-Linux fallback: the rewrite's ioctl path is Linux
// only (the toolchain, CI and the dev machine all run Linux, PRD §6 notes the
// sandbox workaround is Linux-centred), but this stub keeps the package
// buildable under any GOOS so a stray `GOOS=windows go build ./...` doesn't
// fail on an undefined symbol. A non-Linux build simply can't probe the tty
// size this way and falls back to the 80-column default in rendererWidth.
func ioctlWinSize(fd uintptr) (cols int, ok bool) {
	_ = fd
	return 0, false
}
