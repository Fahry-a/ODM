//! Rust port of ODM's `internal/ui` package: the pacman/ILoveCandy bar, the
//! fixed-column task-line renderer, the live TTY redraw loop and the prompts.
//! This is the only layer that writes to the screen.

pub mod ansi;
pub mod bar;
pub mod clock;
pub mod color;
pub mod confirm;
pub mod progress;
pub mod render;
pub mod summary;
pub mod termsize;

pub use ansi::{display_width, fit_width, pad_to_cells, truncate_name_to, truncate_visible_width};
pub use bar::{bar, bar_indeterminate, colorize_bar, BAR_WIDTH};
pub use clock::bounce_position;
pub use confirm::{confirm_batch, confirm_single, rows_from_plan, FileRow};
pub use progress::Renderer;
pub use render::{render_task_line, LineLayout, COL_ETA, COL_GLYPH, COL_PCT, COL_SIZE, COL_SPEED};
pub use summary::{render_summary_width, status_glyph, format_file_size, format_file_size_short, format_speed, format_duration};
pub use termsize::{renderer_size, terminal_size, MIN_TERMINAL_HEIGHT, MIN_TERMINAL_WIDTH};
