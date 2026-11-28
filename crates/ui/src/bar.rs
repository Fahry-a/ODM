//! Rust port of `internal/ui/bar.go` — the fixed-width pacman/ILoveCandy bar.
//! All functions are pure; the renderer owns the animation tick.

use super::color::{CYAN, GREEN, RESET, YELLOW};

/// Visual width of the bar (cells) on wide terminals.
pub const BAR_WIDTH: usize = 30;
const PAC_FACE: char = 'c'; // small, mouth closed — between dots
const PAC_FACE_ALT: char = 'C'; // big, mouth open — eating a dot

/// `bar` — static indeterminate layout (pacman centred when size unknown).
pub fn bar(done: i64, total: i64, width: usize) -> String {
    bar_indeterminate(done, total, width, -1, "")
}

/// `barIndeterminate` — `bar` with an explicit bounce position. `pos < 0`
/// parks the face in the middle; the prefix counts toward the width budget.
pub fn bar_indeterminate(done: i64, total: i64, width: usize, pos: i64, prefix: &str) -> String {
    let width = width.max(2);
    if total <= 0 {
        let p = if pos < 0 { (width / 2) as i64 } else { pos.clamp(0, (width - 1) as i64) };
        return bar_line(p as usize, width, prefix);
    }
    if done >= total {
        return "-".repeat(width);
    }
    let eaten = ((done as f64 / total as f64) * width as f64) as usize;
    bar_line(eaten.min(width - 1), width, prefix)
}

/// `barLine` — prefix + eaten dashes + face + a fixed alternating dot pattern
/// anchored to absolute cells, padded to exactly `width` display cells. The
/// face is big ('C') on even cells (sitting on a dot) and small ('c') between.
fn bar_line(eaten: usize, width: usize, prefix: &str) -> String {
    let face = if eaten % 2 == 0 { PAC_FACE_ALT } else { PAC_FACE };
    let p_len = prefix.chars().count();
    let avail = width.saturating_sub(p_len);
    if avail < 1 {
        return format!("{}{}{}", prefix, "-".repeat(eaten), face);
    }
    if eaten >= avail {
        return format!("{}{}", prefix, "-".repeat(avail));
    }
    let mut s = String::with_capacity(width);
    s.push_str(prefix);
    for _ in 0..eaten {
        s.push('-');
    }
    s.push(face);
    for pos in eaten + 1..avail {
        s.push(if pos % 2 == 0 { 'o' } else { ' ' });
    }
    while s.chars().count() < width {
        s.push(' ');
    }
    s
}

/// `colorizeBar` — face→yellow, dashes→green, dots→cyan (post-pass so the
/// column padding is computed before ANSI codes are added).
pub fn colorize_bar(bar_text: &str, use_color: bool) -> String {
    if !use_color {
        return bar_text.to_string();
    }
    let mut out = String::with_capacity(bar_text.len() + 20);
    for ch in bar_text.chars() {
        match ch {
            'c' | 'C' => {
                out.push_str(YELLOW);
                out.push(ch);
                out.push_str(RESET);
            }
            '-' => {
                out.push_str(GREEN);
                out.push(ch);
                out.push_str(RESET);
            }
            'o' => {
                out.push_str(CYAN);
                out.push(ch);
                out.push_str(RESET);
            }
            _ => out.push(ch),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fixed_width_and_anchored_dots() {
        for w in 2..40 {
            for done in 0..100 {
                let s = bar_indeterminate(done, 100, w, -1, "");
                assert_eq!(s.chars().count(), w, "width {w} done {done}");
            }
        }
    }

    #[test]
    fn pacman_face_size_follows_eat_position() {
        let even = bar_indeterminate(10, 100, 30, -1, ""); // eaten=3 → odd → small
        assert!(even.contains('c'));
        let odd = bar_indeterminate(20, 100, 30, -1, ""); // eaten=6 → even → big
        assert!(odd.contains('C'));
    }

    #[test]
    fn fully_eaten_is_all_dashes() {
        assert_eq!(bar(100, 100, 30), "-".repeat(30));
    }

    #[test]
    fn prefix_counts_toward_width() {
        let s = bar_indeterminate(0, 10, 10, -1, "x4 ");
        assert_eq!(s.chars().count(), 10);
        assert!(s.starts_with("x4 "));
    }

    #[test]
    fn colorize_marks_face_dashes_and_dots() {
        let s = colorize_bar("C-o ", true);
        assert!(s.contains(YELLOW));
        assert!(s.contains(GREEN));
        assert!(s.contains(CYAN));
        assert_eq!(colorize_bar("C-o ", false), "C-o ");
    }
}
