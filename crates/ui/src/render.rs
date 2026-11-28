//! Rust port of `internal/ui/render.go` — the pure per-file task-line layout.
//! Fixed column widths keep the bar glued to the right edge across frames.

use download::{ProgressView, TaskState};

use super::ansi::{fit_width, pad_to_cells, truncate_name_to};
use super::bar::{bar_indeterminate, colorize_bar};
use super::color::{state_color, GREEN, GREY, RED, RESET, YELLOW};
use super::summary::{format_duration, format_file_size_short, format_speed, status_glyph};

pub const COL_GLYPH: usize = 2;
pub const COL_SIZE: usize = 14;
pub const COL_SPEED: usize = 12;
pub const COL_ETA: usize = 8;
pub const COL_CONNS: usize = 6;
pub const COL_PCT: usize = 4;

const UNKNOWN_GLYPH: &str = "?";

/// Which column set a line renders, from the terminal width (`layout_for`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LineLayout {
    Full,
    NoSpeedEta,
    NameBarPct,
    NamePct,
    Pct,
}

/// `renderTaskLine` — format one per-file line. `indeterminate_pos` drives the
/// sizeless bar animation (-1 = static centre); the face animation itself is
/// position-driven, so no clock input is needed here.
pub fn render_task_line(
    v: &ProgressView,
    use_color: bool,
    indeterminate_pos: i64,
    mut name_width: usize,
    mut bar_width: usize,
    layout: LineLayout,
) -> String {
    if name_width < 4 && layout != LineLayout::Pct {
        name_width = 4;
    }
    if bar_width < 2
        && matches!(layout, LineLayout::Full | LineLayout::NoSpeedEta | LineLayout::NameBarPct)
    {
        bar_width = 2;
    }

    // Status glyph column: always 2 cells (glyph + space).
    let (glyph, glyph_col) = status_glyph(v.state);
    let mut glyph_cell = format!("{glyph} ");
    if use_color && !glyph_col.is_empty() {
        glyph_cell = format!("{glyph_col}{glyph} {RESET}");
    }

    // Name cell, padded by display cells (CJK/emoji count double).
    let name = if v.filename.is_empty() { &v.url } else { &v.filename };
    let name_cell = pad_to_cells(&truncate_name_to(name, name_width), name_width);
    let name_cell = if let Some(c) = color_cell(v.state, use_color) {
        format!("{c}{name_cell}{RESET}")
    } else {
        name_cell
    };

    // Size / speed / ETA.
    let mut size = format_file_size_short(v.bytes_done);
    if v.total_size > 0 {
        size.push('/');
        size.push_str(&format_file_size_short(v.total_size));
    } else if v.total_size < 0 {
        size.push('/');
        size.push_str(UNKNOWN_GLYPH);
    }
    if v.total_size <= 0 && v.state == TaskState::Completed && v.bytes_done > 0 {
        size = format!("{}/{}", format_file_size_short(v.bytes_done), format_file_size_short(v.bytes_done));
    }
    let (mut speed, mut eta) = (format_speed(v.speed), format_duration(v.eta));
    if v.state == TaskState::Completed {
        // Keep the cells inside the fixed grid so completed rows' bars stay in
        // the same column as active rows above/below.
        speed.clear();
        eta.clear();
    }

    // Fixed-width connection indicator before the bar bracket.
    let mut conn_display = " ".repeat(COL_CONNS);
    if v.connections > 0 && v.state != TaskState::Completed {
        let conn_str = format!("[x{}]", v.connections);
        conn_display = format!("{}{}", conn_str, " ".repeat(COL_CONNS.saturating_sub(conn_str.len())));
    }

    let bar = bar_indeterminate(v.bytes_done, v.total_size, bar_width, indeterminate_pos, "");
    let pct_str = if v.total_size > 0 {
        let pct = ((v.bytes_done as f64 / v.total_size as f64 * 100.0) as i64).clamp(0, 100);
        format!("{pct}%")
    } else {
        UNKNOWN_GLYPH.to_string()
    };
    let pct_col = fit_width(&pct_str, COL_PCT);

    // Completed rows collapse the dead speed/ETA/conn cells into padding after
    // the name so the size sits flush against the bar; total width is unchanged.
    let glue_size_to_bar = |pad: usize| {
        format!(
            "{}{}{}{}  [{}]  {}",
            glyph_cell,
            name_cell,
            " ".repeat(pad.saturating_sub(2)),
            fit_width(&size, COL_SIZE),
            bar,
            pct_col,
        )
    };

    let mut line = match layout {
        LineLayout::Pct => fit_width(&pct_str, name_width),
        LineLayout::NoSpeedEta => {
            if speed.is_empty() && eta.is_empty() {
                glue_size_to_bar(2 + 2 + COL_CONNS)
            } else {
                format!(
                    "{}{}  {}  {}[{}]  {}",
                    glyph_cell,
                    name_cell,
                    fit_width(&size, COL_SIZE),
                    conn_display,
                    bar,
                    pct_col,
                )
            }
        }
        LineLayout::NameBarPct => {
            format!("{}{}  {}[{}]  {}", glyph_cell, name_cell, conn_display, bar, pct_col)
        }
        LineLayout::NamePct => format!("{}{}  {}", glyph_cell, name_cell, pct_col),
        LineLayout::Full => {
            if speed.is_empty() && eta.is_empty() {
                glue_size_to_bar(2 + 2 + COL_SPEED + 2 + COL_ETA + 2 + COL_CONNS)
            } else {
                format!(
                    "{}{}  {}  {}  {}  {}[{}]  {}",
                    glyph_cell,
                    name_cell,
                    fit_width(&size, COL_SIZE),
                    fit_width(&speed, COL_SPEED),
                    fit_width(&eta, COL_ETA),
                    conn_display,
                    bar,
                    pct_col,
                )
            }
        }
    };

    if use_color {
        let pct_color = match v.state {
            TaskState::Active | TaskState::Retrying => Some(YELLOW),
            TaskState::Completed => Some(GREEN),
            TaskState::Error => Some(RED),
            TaskState::Paused | TaskState::Queued => Some(GREY),
        };
        if let Some(c) = pct_color {
            line = line.replacen(&pct_col, &format!("{c}{pct_col}{RESET}"), 1);
        }
        if matches!(layout, LineLayout::Full | LineLayout::NoSpeedEta | LineLayout::NameBarPct) {
            line = colorize_bar_in_line(&line);
        }
    }

    // Error badge — the only variable-width tail, present only on errored rows.
    if v.errors > 0
        && matches!(layout, LineLayout::Full | LineLayout::NoSpeedEta | LineLayout::NameBarPct)
    {
        let badge = format!(" e{}", v.errors);
        line.push_str(&if use_color { format!("{RED}{badge}{RESET}") } else { badge });
    }
    line
}

fn color_cell(state: TaskState, use_color: bool) -> Option<&'static str> {
    let c = state_color(state, use_color);
    if c.is_empty() { None } else { Some(c) }
}

/// Colour the characters inside the last `[...]` bracket pair of a line.
fn colorize_bar_in_line(line: &str) -> String {
    let open = match line.rfind('[') {
        Some(i) => i,
        None => return line.to_string(),
    };
    let close_rel = match line[open..].find(']') {
        Some(i) => i,
        None => return line.to_string(),
    };
    let close = open + close_rel;
    format!("{}{}{}", &line[..open + 1], colorize_bar(&line[open + 1..close], true), &line[close..])
}

/// `infoFixedWidth` — width of everything after the name in the FULL layout
/// that does not depend on the bar width.
pub const INFO_FIXED_WIDTH: usize = 2 + COL_SIZE + 2 + COL_SPEED + 2 + COL_ETA + 2 + COL_CONNS + 1 + 1 + 2 + COL_PCT;

/// `infoBlockWidthFor` — glyph + fixed info + bar.
pub fn info_block_width_for(bar_width: usize) -> usize {
    COL_GLYPH + INFO_FIXED_WIDTH + bar_width
}

/// Keep `display_width` in this module's API for the renderer's truncation.
pub use super::ansi::display_width as line_display_width;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ansi::display_width;
    use std::time::Duration;

    fn view(state: TaskState, done: i64, total: i64, speed: i64, connections: i32) -> ProgressView {
        ProgressView {
            id: "odm-1".into(),
            url: "http://x/file.bin".into(),
            final_url: "http://x/file.bin".into(),
            filename: "file.bin".into(),
            state,
            total_size: total,
            bytes_done: done,
            speed,
            connections,
            eta: Duration::from_secs(42),
            errors: 0,
            retries: 0,
            single_stream: false,
        }
    }

    #[test]
    fn fixed_columns_pin_bar_and_percent() {
        let a = render_task_line(&view(TaskState::Active, 100, 1000, 1024, 4), false, -1, 20, 30, LineLayout::Full);
        let b = render_task_line(&view(TaskState::Active, 900000, 1000000, 1024 * 1024, 32), false, -1, 20, 30, LineLayout::Full);
        // Same name width + bar width ⇒ the bar brackets land at the same cell.
        assert_eq!(a.find('['), b.find('['));
        assert_eq!(a.chars().count(), b.chars().count());
    }

    #[test]
    fn completed_row_keeps_grid_and_blanks_speed_eta() {
        let s = render_task_line(&view(TaskState::Completed, 100, 100, 0, 0), false, -1, 20, 30, LineLayout::Full);
        assert!(!s.contains("B/s"));
        assert!(!s.contains("ETA"));
        assert!(s.contains("100%"));
        assert!(s.contains('+'));
    }

    #[test]
    fn sizeless_shows_unknown_percent() {
        let s = render_task_line(&view(TaskState::Active, 100, -1, 0, 1), false, -1, 20, 30, LineLayout::Full);
        assert!(s.contains('?'));
    }

    #[test]
    fn error_badge_only_when_errors() {
        let mut v = view(TaskState::Error, 10, 100, 0, 1);
        v.errors = 3;
        let s = render_task_line(&v, false, -1, 20, 30, LineLayout::Full);
        assert!(s.ends_with(" e3"));
    }

    #[test]
    fn narrow_layouts_still_fit() {
        let v = view(TaskState::Active, 50, 100, 100, 1);
        for layout in [LineLayout::Full, LineLayout::NoSpeedEta, LineLayout::NameBarPct, LineLayout::NamePct, LineLayout::Pct] {
            let s = render_task_line(&v, false, -1, 12, 10, layout);
            // The renderer truncates to the terminal width; here just assert
            // each tier still renders its percent column.
            assert!(s.contains("50%"), "{layout:?}: {s}");
            assert!(display_width(&s) > 0);
        }
    }
}
