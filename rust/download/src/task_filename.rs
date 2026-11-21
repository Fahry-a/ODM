//! Rust port of ODM's `internal/download/task_filename.go` — uniqueName,
//! sizeOrUnknown, formatSegSize, flattenFilename, deriveFilename.

use std::path::Path;

/// `unique_name` — returns dir/name rewritten to base.N.ext with the lowest
/// N≥1 that doesn't exist yet.
pub fn unique_name(dir: &str, name: &str) -> String {
    let ext = Path::new(name).extension().map(|e| format!(".{}", e.to_string_lossy())).unwrap_or_default();
    let base = name.strip_suffix(&ext).unwrap_or(name).to_string();
    let mut i = 1u32;
    loop {
        let candidate = format!("{base}.{i}{ext}");
        if !Path::new(dir).join(&candidate).exists() {
            return candidate;
        }
        i += 1;
    }
}

/// `size_or_unknown` — renders size for log lines: "?" when unknown (<0).
pub fn size_or_unknown(size: i64) -> String {
    if size < 0 {
        "?".to_string()
    } else {
        size.to_string()
    }
}

/// `format_seg_size` — compact byte-count rendering for log lines.
pub fn format_seg_size(b: i64) -> String {
    const UNIT: f64 = 1024.0;
    if b < UNIT as i64 {
        return format!("{b} B");
    }
    let mut val = b as f64;
    let units = ["KiB", "MiB", "GiB", "TiB"];
    let mut idx = -1;
    while val >= UNIT && idx < units.len() as i32 - 1 {
        val /= UNIT;
        idx += 1;
    }
    format!("{:.1} {}", val, units[idx as usize])
}

/// `flatten_filename` — clamps a server-controlled filename to a single path
/// component (no separators, no escape from Dir).
pub fn flatten_filename(name: &str) -> String {
    let name: String = name
        .chars()
        .map(|r| if r == '/' || r == '\\' { '_' } else { r })
        .collect();
    if name.is_empty() || name == "." || name == ".." {
        return "download.bin".to_string();
    }
    // A trailing "..foo" is a valid filename; only exact dot segments are
    // dangerous. After separator flattening no path element boundary remains,
    // so the base check is belt-and-braces.
    if let Some(base) = Path::new(&name).file_name() {
        let base = base.to_string_lossy();
        if base != "." && base != ".." && base != name {
            // fallthrough: keep `name` unless it differs
        }
    }
    if Path::new(&name).parent().map(|p| !p.as_os_str().is_empty()).unwrap_or(false) {
        // contains separator → already flattened
    }
    // After flattening there are no separators; if the name is "." or ".." we
    // already handled it. A ".."-prefixed name can't escape (no sep).
    name
}

/// `derive_filename` — picks an output name from the URL path, or falls back
/// to the --output override / "download.bin".
pub fn derive_filename(final_url: &str, override_: &str) -> String {
    if !override_.is_empty() {
        return override_.to_string();
    }
    let u = final_url.split('?').next().unwrap_or(final_url);
    if let Some(i) = u.rfind('/') {
        let name = &u[i + 1..];
        if !name.is_empty() {
            return flatten_filename(name);
        }
    }
    "download.bin".to_string()
}
