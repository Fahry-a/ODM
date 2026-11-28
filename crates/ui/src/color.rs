//! Rust port of `internal/ui/color.go` — ANSI colour constants and the
//! state→colour mapping shared by the renderer, bar and summary.

use download::TaskState;

pub const RESET: &str = "\x1b[0m";
pub const GREEN: &str = "\x1b[32m";
pub const YELLOW: &str = "\x1b[33m";
pub const RED: &str = "\x1b[31m";
pub const GREY: &str = "\x1b[90m";
pub const CYAN: &str = "\x1b[36m";
pub const MAGENTA: &str = "\x1b[35m";

/// `stateColor` — a state's foreground colour, or "" when colours are off.
pub fn state_color(s: TaskState, use_color: bool) -> &'static str {
    if !use_color {
        return "";
    }
    match s {
        TaskState::Completed => GREEN,
        TaskState::Active => YELLOW,
        TaskState::Retrying | TaskState::Error => RED,
        TaskState::Queued | TaskState::Paused => GREY,
    }
}
