//! Rust port of `internal/ui/summary.go` — byte/speed/duration humanisers,
//! state glyphs and the aggregate summary line.

use std::time::Duration;

use download::TaskState;

use super::ansi::{display_width, fit_width};
use super::bar::{bar_indeterminate, colorize_bar, BAR_WIDTH};
use super::color::{CYAN, GREEN, GREY, RED, RESET, YELLOW};
use super::render::{COL_ETA, COL_PCT, COL_SPEED};

const UNKNOWN_GLYPH: &str = "?";

/// `FormatFileSize` — humanise a byte count (binary units, KiB/MiB/…).
pub fn format_file_size(b: i64) -> String {
    if b < 0 {
        return UNKNOWN_GLYPH.to_string();
    }
    if b < 1024 {
        return format!("{b} B");
    }
    let mut val = b as f64;
    let units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    let mut idx = -1i32;
    while val >= 1024.0 && idx < units.len() as i32 - 1 {
        val /= 1024.0;
        idx += 1;
    }
    // %.1f would print "1024.0 XiB" for values just under the next unit;
    // promote instead so the column never overflows.
    if idx < units.len() as i32 - 1 && val + 0.05 >= 1024.0 {
        val /= 1024.0;
        idx += 1;
    }
    format!("{val:.1} {}", units[idx as usize])
}

/// `FormatFileSizeShort` — single-letter suffixes (K/M/G/T/P) for two-part
/// displays like "42.0M/256.0M".
pub fn format_file_size_short(b: i64) -> String {
    if b < 0 {
        return UNKNOWN_GLYPH.to_string();
    }
    if b < 1024 {
        return format!("{b}B");
    }
    let mut val = b as f64;
    let units = ["K", "M", "G", "T", "P"];
    let mut idx = -1i32;
    while val >= 1024.0 && idx < units.len() as i32 - 1 {
        val /= 1024.0;
        idx += 1;
    }
    if idx < units.len() as i32 - 1 && val + 0.05 >= 1024.0 {
        val /= 1024.0;
        idx += 1;
    }
    format!("{val:.1}{}", units[idx as usize])
}

/// `FormatSpeed` — humanise bytes/sec; negative reads as unknown.
pub fn format_speed(bps: i64) -> String {
    if bps < 0 {
        return UNKNOWN_GLYPH.to_string();
    }
    format!("{}/s", format_file_size(bps))
}

/// `FormatDuration` — fixed-width HH:MM:SS (8 cells), capped at 99:59:59.
/// Non-positive durations render the shared unknown glyph right-aligned.
pub fn format_duration(d: Duration) -> String {
    if d.is_zero() {
        return format!("{:>7}", UNKNOWN_GLYPH);
    }
    let s = d.as_secs();
    let h = s / 3600;
    let m = (s % 3600) / 60;
    let sec = s % 60;
    if h > 99 {
        return "99:59:59".to_string();
    }
    format!("{h:02}:{m:02}:{sec:02}")
}

/// `statusGlyph` — one-cell ASCII state symbol + its colour. ASCII on purpose:
/// ambiguous-width Unicode glyphs render two cells wide on some terminals and
/// break the one-logical-line-per-physical-row redraw contract.
pub fn status_glyph(s: TaskState) -> (&'static str, &'static str) {
    match s {
        TaskState::Active => (">", YELLOW),
        TaskState::Retrying => ("!", RED),
        TaskState::Error => ("x", RED),
        TaskState::Completed => ("+", GREEN),
        TaskState::Paused => ("|", GREY),
        TaskState::Queued => (".", GREY),
    }
}

/// `RenderSummary` — fixed-width summary (non-TTY default).
pub fn render_summary(
    completed: usize,
    total: usize,
    speed_bps: i64,
    eta: Duration,
    elapsed: Duration,
    bytes_done: i64,
    total_size: i64,
    use_color: bool,
) -> String {
    render_summary_width(completed, total, speed_bps, eta, elapsed, bytes_done, total_size, use_color, 0, BAR_WIDTH)
}

/// `RenderSummaryWidth` — right-aligned summary for a TTY of `term_width`.
pub fn render_summary_width(
    completed: usize,
    total: usize,
    speed_bps: i64,
    eta: Duration,
    elapsed: Duration,
    bytes_done: i64,
    total_size: i64,
    use_color: bool,
    term_width: usize,
    bar_width: usize,
) -> String {
    let bar_width = bar_width.max(2);
    let sp = fit_width(&format_speed(speed_bps), COL_SPEED);
    let eta_str = fit_width(&format_duration(eta), COL_ETA);
    let el_str = if !elapsed.is_zero() {
        fit_width(&format!("+{}", format_duration(elapsed)), COL_ETA + 1)
    } else {
        String::new()
    };
    let pct_str = if total_size > 0 {
        let pct = ((bytes_done as f64 / total_size as f64 * 100.0) as i64).clamp(0, 100);
        format!("{pct}%")
    } else {
        UNKNOWN_GLYPH.to_string()
    };
    let pct_col = fit_width(&pct_str, COL_PCT);
    let bar = colorize_bar(&bar_indeterminate(bytes_done, total_size, bar_width, -1, ""), use_color);

    let left_text = format!("Total: {completed}/{total} completed");
    let mut bytes_str = format_file_size(bytes_done);
    if total_size > 0 {
        bytes_str.push('/');
        bytes_str.push_str(&format_file_size(total_size));
    }
    let pct_colored = if use_color { format!("{GREEN}{pct_col}{RESET}") } else { pct_col.clone() };
    let left_colored = if use_color { format!("{GREEN}{left_text}{RESET}") } else { left_text.clone() };

    // When every task finished, "0 B/s ETA ?" is noise: keep only the elapsed.
    let all_done = total > 0 && completed >= total;

    let mut segs: Vec<String> = Vec::new();
    if total_size > 0 && !bytes_str.is_empty() && !all_done {
        segs.push(bytes_str);
    }
    if !all_done {
        segs.push(format!("{sp}  ETA {eta_str}"));
    }
    if !el_str.is_empty() {
        segs.push(el_str.clone());
    }
    let mut right = segs.join("  ");
    if !right.is_empty() {
        right.push_str("  ");
    }
    right.push_str(&format!("[{bar}]  {pct_colored}"));
    if use_color {
        if !all_done {
            right = right.replacen(&sp, &format!("{YELLOW}{sp}{RESET}"), 1);
        }
        if !el_str.is_empty() {
            right = right.replacen(&el_str, &format!("{CYAN}{el_str}{RESET}"), 1);
        }
    }

    if term_width > 0 {
        let pad = term_width as i64 - display_width(&left_colored) as i64 - display_width(&right) as i64 - 1;
        if pad >= 2 {
            return format!("{}{}{}", left_colored, " ".repeat(pad as usize), right);
        }
        let mut compact_right = format!("{sp}  ETA {eta_str}  [{bar}]  {pct_colored}");
        if use_color {
            compact_right = compact_right.replacen(&sp, &format!("{YELLOW}{sp}{RESET}"), 1);
        }
        let compact_pad = term_width as i64 - display_width(&left_colored) as i64 - display_width(&compact_right) as i64 - 1;
        if compact_pad >= 1 {
            return format!("{}{}{}", left_colored, " ".repeat(compact_pad as usize), compact_right);
        }
        let left_short = format!("Total: {completed}/{total}");
        let left_short_colored = if use_color { format!("{GREEN}{left_short}{RESET}") } else { left_short };
        let min_right = format!("[{bar}]  {pct_colored}");
        let min_pad = term_width as i64 - display_width(&left_short_colored) as i64 - display_width(&min_right) as i64 - 1;
        if min_pad >= 1 {
            return format!("{}{}{}", left_short_colored, " ".repeat(min_pad as usize), min_right);
        }
        return pct_colored;
    }

    if all_done {
        if use_color {
            return format!("{left_colored}  {el_str}  [{bar}]  {pct_colored}");
        }
        return format!("{left_text}  {el_str}  [{bar}]  {pct_col}");
    }
    if use_color {
        format!("{left_colored}  |  {YELLOW}{sp}{RESET}  |  ETA {eta_str}  {el_str}  [{bar}]  {pct_colored}")
    } else {
        format!("{left_text}  |  {sp}  |  ETA {eta_str}  {el_str}  [{bar}]  {pct_col}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn humanizes_sizes_without_overflowing_units() {
        assert_eq!(format_file_size(0), "0 B");
        assert_eq!(format_file_size(1023), "1023 B");
        assert_eq!(format_file_size(1024), "1.0 KiB");
        assert_eq!(format_file_size_short(1024 * 1024), "1.0M");
        assert_eq!(format_speed(-1), "?");
        assert_eq!(format_speed(2048), "2.0 KiB/s");
    }

    #[test]
    fn duration_is_fixed_width_and_capped() {
        assert_eq!(format_duration(Duration::ZERO), "      ?");
        assert_eq!(format_duration(Duration::from_secs(5 * 3600 + 61)), "05:01:01");
        assert_eq!(format_duration(Duration::from_secs(100 * 3600)), "99:59:59");
    }

    #[test]
    fn all_done_summary_drops_speed_and_eta() {
        let s = render_summary(2, 2, 0, Duration::ZERO, Duration::from_secs(3), 2048, 2048, false);
        assert!(s.contains("Total: 2/2 completed"));
        assert!(!s.contains("ETA"));
        assert!(!s.contains("B/s"));
        assert!(s.contains("+00:00:03"));
    }
}
