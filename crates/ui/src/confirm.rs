//! Rust port of `internal/ui/confirm.go` — the Y/n confirmation prompts.
//! Input/output are injected so every prompt is testable without a terminal.

use std::collections::HashMap;
use std::io::{BufRead, Write};

use scheduler::Plan;

use super::ansi::{display_width, pad_to_cells, truncate_name_to};
use super::color::{CYAN, GREEN, MAGENTA, RESET, YELLOW};
use super::format_file_size;

/// `ConfirmAsk` — read a Y/n answer, re-prompting on typos. Empty/y/yes confirms,
/// n/no declines. EOF with no answer is a silent cancel.
pub fn confirm_ask(inp: &mut dyn BufRead, out: &mut dyn Write, prompt: &str) -> std::io::Result<bool> {
    let mut line = String::new();
    loop {
        write!(out, "{prompt}")?;
        out.flush()?;
        line.clear();
        if inp.read_line(&mut line)? == 0 {
            return Ok(false); // EOF → silent cancel
        }
        match line.trim().to_lowercase().as_str() {
            "" | "y" | "yes" => return Ok(true),
            "n" | "no" => return Ok(false),
            _ => writeln!(out, "Please answer [Y/n] or press Ctrl-D to cancel.")?,
        }
    }
}

/// `ConfirmSingle` — the single-file prompt.
pub fn confirm_single(
    inp: &mut dyn BufRead,
    out: &mut dyn Write,
    filename: &str,
    dest: &str,
    size: i64,
    conns: i32,
    profile: &str,
    reason: &str,
    use_color: bool,
) -> std::io::Result<bool> {
    if use_color {
        writeln!(out, "  {YELLOW}File       :{RESET} {CYAN}{filename}{RESET}")?;
        writeln!(out, "  {YELLOW}Size       :{RESET} {GREEN}{}{RESET}", format_file_size(size))?;
        writeln!(out, "  {YELLOW}Connections:{RESET} {MAGENTA}{conns} parallel{RESET}")?;
        if let Some(p) = profile_suffix(profile, reason) {
            writeln!(out, "  {YELLOW}Engine     :{RESET} {CYAN}{}{RESET}", p.trim_start())?;
        }
        writeln!(out, "  {YELLOW}Destination:{RESET} {dest}")?;
        writeln!(out)?;
        return confirm_ask(inp, out, &format!("{YELLOW}Continue?{RESET} [Y/n] "));
    }
    writeln!(out, "File       : {filename}")?;
    writeln!(out, "Size       : {}", format_file_size(size))?;
    writeln!(out, "Connections: {conns} parallel")?;
    if let Some(p) = profile_suffix(profile, reason) {
        writeln!(out, "Engine     : {}", p.trim_start())?;
    }
    writeln!(out, "Destination: {dest}")?;
    writeln!(out)?;
    confirm_ask(inp, out, "Continue? [Y/n] ")
}

/// One line of the batch prompt's file list.
#[derive(Debug, Clone)]
pub struct FileRow {
    pub name: String,
    pub size: i64,
    pub profile: String,
    pub reason: String,
}

/// `ConfirmBatch` — the multi-file prompt.
pub fn confirm_batch(
    inp: &mut dyn BufRead,
    out: &mut dyn Write,
    rows: &[FileRow],
    conns_per_file: i32,
    parallel_files: usize,
    total_files: usize,
    use_color: bool,
) -> std::io::Result<bool> {
    let total_size: i64 = rows.iter().map(|r| r.size).sum();
    if use_color {
        writeln!(
            out,
            "  {GREEN}ODM will download{RESET} {YELLOW}{total_files} files{RESET} (total ~{CYAN}{}{RESET})",
            format_file_size(total_size),
        )?;
        if conns_per_file <= 1 {
            writeln!(out, "  {YELLOW}Allocation:{RESET} 1 connection/file, {MAGENTA}{parallel_files} files{RESET} running in parallel (rest queued)")?;
        } else {
            writeln!(out, "  {YELLOW}Allocation:{RESET} {MAGENTA}{conns_per_file} connections/file{RESET}, {MAGENTA}{parallel_files} files{RESET} running in parallel (rest queued)")?;
        }
        writeln!(out)?;
        for (i, r) in rows.iter().enumerate() {
            let size_str = format_file_size(r.size);
            let size_col = format!("{}{}", " ".repeat(14usize.saturating_sub(display_width(&size_str))), size_str);
            writeln!(
                out,
                "    {GREEN}[{}]{RESET} {}{CYAN}{}{RESET}{}",
                i + 1,
                pad_to_cells(&truncate_name_to(&r.name, 40), 40),
                size_col,
                profile_suffix(&r.profile, &r.reason).unwrap_or_default(),
            )?;
        }
        writeln!(out)?;
        return confirm_ask(inp, out, &format!("{YELLOW}Continue?{RESET} [Y/n] "));
    }
    writeln!(out, "ODM will download {total_files} files (total ~{})", format_file_size(total_size))?;
    if conns_per_file <= 1 {
        writeln!(out, "Allocation: 1 connection/file, {parallel_files} files running in parallel (rest queued automatically)")?;
    } else {
        writeln!(out, "Allocation: {conns_per_file} connections/file, {parallel_files} files running in parallel (rest queued automatically)")?;
    }
    writeln!(out)?;
    for (i, r) in rows.iter().enumerate() {
        let size_str = format_file_size(r.size);
        let size_col = format!("{}{}", " ".repeat(14usize.saturating_sub(display_width(&size_str))), size_str);
        writeln!(
            out,
            "  [{}] {} {}{}",
            i + 1,
            pad_to_cells(&truncate_name_to(&r.name, 40), 40),
            size_col,
            profile_suffix(&r.profile, &r.reason).unwrap_or_default(),
        )?;
    }
    writeln!(out)?;
    confirm_ask(inp, out, "Continue? [Y/n] ")
}

/// `RowsFromPlan` — build file rows from a Balancer plan plus probed sizes and
/// resolved smart-profile decisions.
pub fn rows_from_plan(
    plan: &Plan,
    sizes: &HashMap<String, i64>,
    profiles: &HashMap<String, String>,
    reasons: &HashMap<String, String>,
    show_profiles: bool,
) -> Vec<FileRow> {
    let mut out = Vec::with_capacity(plan.parallel.len() + plan.queued.len());
    for a in plan.parallel.iter().chain(plan.queued.iter()) {
        let p = if show_profiles { profiles.get(&a.url).cloned().unwrap_or_default() } else { String::new() };
        out.push(FileRow {
            name: short_name(&a.url),
            size: sizes.get(&a.url).copied().unwrap_or(-1),
            profile: p,
            reason: reasons.get(&a.url).cloned().unwrap_or_default(),
        });
    }
    out
}

fn short_name(u: &str) -> String {
    u.rsplit_once('/').map(|(_, tail)| tail).unwrap_or(u).to_string()
}

fn profile_suffix(profile: &str, reason: &str) -> Option<String> {
    if profile.is_empty() {
        return None;
    }
    if reason.is_empty() {
        Some(format!(" [{profile}]"))
    } else {
        Some(format!(" [{profile} — {reason}]"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::BufReader;

    fn answer(s: &str) -> BufReader<&[u8]> {
        BufReader::new(s.as_bytes())
    }

    #[test]
    fn empty_and_yes_confirm_no_declines() {
        let mut out = Vec::new();
        assert!(confirm_ask(&mut answer("\n"), &mut out, "? ").unwrap());
        assert!(confirm_ask(&mut answer("yes\n"), &mut out, "? ").unwrap());
        assert!(!confirm_ask(&mut answer("n\n"), &mut out, "? ").unwrap());
    }

    #[test]
    fn typo_reprompts_until_valid() {
        let mut out = Vec::new();
        assert!(confirm_ask(&mut answer("sure\ny\n"), &mut out, "? ").unwrap());
        let text = String::from_utf8(out).unwrap();
        assert_eq!(text.matches("Please answer").count(), 1);
    }

    #[test]
    fn eof_is_silent_cancel() {
        let mut out = Vec::new();
        assert!(!confirm_ask(&mut answer(""), &mut out, "? ").unwrap());
    }

    #[test]
    fn single_prompt_snapshot_shape() {
        let mut out = Vec::new();
        assert!(confirm_single(&mut answer("\n"), &mut out, "big.iso", "/tmp/big.iso", -1, 1, "", "", false).unwrap());
        let text = String::from_utf8(out).unwrap();
        assert!(text.contains("File       : big.iso"));
        assert!(text.contains("Size       : ?"));
        assert!(text.contains("Connections: 1 parallel"));
        assert!(text.ends_with("Continue? [Y/n] "));
    }

    #[test]
    fn profile_suffix_includes_reason() {
        assert_eq!(profile_suffix("aria2c", "no h2"), Some(" [aria2c — no h2]".into()));
        assert_eq!(profile_suffix("", "x"), None);
    }
}
