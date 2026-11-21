//! Rust port of `internal/ui/ansi.go` — ANSI-aware string primitives: terminal
//! cell widths (wide runes count double), escape-aware truncation and padding.

use super::color::RESET;

/// `runeWidth` — terminal cells occupied by a char (wcwidth's East Asian Wide
/// ranges, implemented inline to avoid a dependency).
pub fn rune_width(r: char) -> usize {
    let u = r as u32;
    if u >= 0x1100
        && (u <= 0x115f
            || u == 0x2329
            || u == 0x232a
            || (u >= 0x2e80 && u <= 0xa4cf && u != 0x303f)
            || (u >= 0xac00 && u <= 0xd7a3)
            || (u >= 0xf900 && u <= 0xfaff)
            || (u >= 0xfe10 && u <= 0xfe19)
            || (u >= 0xfe30 && u <= 0xfe6f)
            || (u >= 0xff00 && u <= 0xff60)
            || (u >= 0xffe0 && u <= 0xffe6)
            || (u >= 0x1f300 && u <= 0x1faff)
            || (u >= 0x1f900 && u <= 0x1f9ff)
            || (u >= 0x20000 && u <= 0x2fffd)
            || (u >= 0x30000 && u <= 0x3fffd))
    {
        2
    } else {
        1
    }
}

/// One span of a string: an ANSI escape sequence (no width) or a visible char.
struct AnsiSeg {
    text: String,
    visible: bool,
    width: usize,
}

/// `scanAnsi` — split into visible chars and complete CSI escape sequences.
/// A malformed/unterminated escape is treated as visible text (never swallows
/// the rest of the line).
fn scan_ansi(s: &str) -> Vec<AnsiSeg> {
    let b = s.as_bytes();
    let mut segs = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] == 0x1b && i + 1 < b.len() && b[i + 1] == b'[' {
            let mut j = i + 2;
            while j < b.len() && (0x20..=0x3f).contains(&b[j]) {
                j += 1;
            }
            if j < b.len() && (0x40..=0x7e).contains(&b[j]) {
                segs.push(AnsiSeg { text: s[i..=j].to_string(), visible: false, width: 0 });
                i = j + 1;
                continue;
            }
        }
        let ch = s[i..].chars().next().unwrap();
        let size = ch.len_utf8();
        segs.push(AnsiSeg { text: s[i..i + size].to_string(), visible: true, width: rune_width(ch) });
        i += size;
    }
    segs
}

/// `displayWidth` — terminal cells occupied by `s`; ANSI sequences are free.
pub fn display_width(s: &str) -> usize {
    scan_ansi(s).iter().filter(|seg| seg.visible).map(|seg| seg.width).sum()
}

/// `truncateVisibleWidth` — cut `s` to the first `width` visible cells,
/// preserving ANSI sequences; appends a reset if colour was active at the cut.
pub fn truncate_visible_width(s: &str, width: usize) -> String {
    if width == 0 {
        return s.to_string();
    }
    let mut out = String::new();
    let mut remaining = width as i64;
    let mut color_active = false;
    for seg in scan_ansi(s) {
        if !seg.visible {
            out.push_str(&seg.text);
            color_active = !seg.text.contains(RESET);
            continue;
        }
        if remaining <= 0 {
            break;
        }
        out.push_str(&seg.text);
        remaining -= seg.width as i64;
        color_active = false;
    }
    if remaining <= 0 && color_active {
        out.push_str(RESET);
    }
    out
}

/// `fitWidth` — right-align `s` into exactly `w` display cells; over-wide
/// values are truncated so they can never shove neighbouring columns.
pub fn fit_width(s: &str, w: usize) -> String {
    if w == 0 {
        return String::new();
    }
    let d = display_width(s);
    if d == w {
        return s.to_string();
    }
    if d > w {
        return truncate_visible_width(s, w);
    }
    format!("{}{}", " ".repeat(w - d), s)
}

/// `truncateNameTo` — cut to `max` display cells, rune-safe and wide-aware,
/// reserving 3 cells for the ellipsis.
pub fn truncate_name_to(name: &str, max: usize) -> String {
    if max == 0 || display_width(name) <= max {
        return name.to_string();
    }
    let mut budget = max.saturating_sub(3) as i64;
    let mut out = String::new();
    for ch in name.chars() {
        let w = rune_width(ch) as i64;
        if budget - w < 0 {
            break;
        }
        out.push(ch);
        budget -= w;
    }
    out + "..."
}

/// `padToCells` — left-justify with spaces to exactly `w` display cells.
pub fn pad_to_cells(s: &str, w: usize) -> String {
    let d = display_width(s);
    if d < w {
        format!("{}{}", s, " ".repeat(w - d))
    } else {
        s.to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wide_runes_count_double() {
        assert_eq!(rune_width('a'), 1);
        assert_eq!(rune_width('世'), 2);
        assert_eq!(rune_width('🦀'), 2);
    }

    #[test]
    fn ansi_sequences_have_zero_width() {
        assert_eq!(display_width("\x1b[32mok\x1b[0m"), 2);
        assert_eq!(display_width("世界"), 4);
    }

    #[test]
    fn fit_width_right_aligns_and_truncates() {
        assert_eq!(fit_width("ab", 5), "   ab");
        assert_eq!(fit_width("abcdef", 3), "abc");
        assert_eq!(fit_width("\x1b[32mab\x1b[0m", 4), "  \x1b[32mab\x1b[0m");
    }

    #[test]
    fn truncate_name_reserves_ellipsis() {
        assert_eq!(truncate_name_to("abcdef", 5), "ab...");
        assert_eq!(truncate_name_to("世界abc", 5), "世...");
    }

    #[test]
    fn malformed_escape_is_visible_text() {
        // "\x1b[" has no final byte, so it is counted as two visible chars.
        assert_eq!(display_width("\x1b["), 2);
    }
}
