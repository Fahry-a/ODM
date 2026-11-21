//! Rust port of `internal/ui/termsize*.go` — terminal size via TIOCGWINSZ
//! (no external crate, matching the Go decision not to add x/term).

pub const MIN_TERMINAL_WIDTH: usize = 80;
pub const MIN_TERMINAL_HEIGHT: usize = 24;

#[repr(C)]
#[derive(Default)]
struct WinSize {
    ws_row: u16,
    ws_col: u16,
    ws_xpixel: u16,
    ws_ypixel: u16,
}

#[cfg(target_os = "linux")]
extern "C" {
    fn ioctl(fd: i32, request: u64, ...) -> i32;
}

#[cfg(target_os = "linux")]
const TIOCGWINSZ: u64 = 0x5413;

/// `terminalSize` — (cols, rows) of the terminal on `fd`, if the ioctl succeeds.
pub fn terminal_size(fd: i32) -> Option<(usize, usize)> {
    #[cfg(target_os = "linux")]
    {
        let mut ws = WinSize::default();
        // SAFETY: ws is a valid, correctly sized buffer for TIOCGWINSZ.
        let rc = unsafe { ioctl(fd, TIOCGWINSZ, &mut ws as *mut WinSize) };
        if rc == 0 && ws.ws_col > 0 && ws.ws_row > 0 {
            return Some((ws.ws_col as usize, ws.ws_row as usize));
        }
    }
    #[cfg(not(target_os = "linux"))]
    let _ = fd;
    None
}

/// `rendererSize` — one column is reserved so a full-width line never wraps;
/// non-TTY callers fall back to the Min* defaults.
pub fn renderer_size(fd: Option<i32>) -> (usize, usize) {
    match fd.and_then(terminal_size) {
        Some((cols, rows)) if cols > 0 => (cols - 1, rows),
        _ => (MIN_TERMINAL_WIDTH - 1, MIN_TERMINAL_HEIGHT),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn non_tty_uses_defaults() {
        assert_eq!(renderer_size(None), (79, 24));
    }

    #[test]
    fn bogus_fd_uses_defaults() {
        assert_eq!(renderer_size(Some(-1)), (79, 24));
    }
}
