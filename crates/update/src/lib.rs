//! Rust port of ODM's `internal/update` package: self-update and
//! version-check logic. Detects the install method (AUR, self-installed via
//! install.sh, or manual) and dispatches to the appropriate update path.

use std::env;
use std::fmt;
use std::fs::OpenOptions;
use std::io::{self, ErrorKind};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const REPO_OWNER: &str = "Fahry-a";
const REPO_NAME: &str = "odm";
const GITHUB_API: &str = "https://api.github.com/repos/Fahry-a/odm/releases/latest";
const HINT_CACHE_FILE: &str = ".last-update-check";
const HINT_CACHE_DIR: &str = ".config/odm";
const HINT_CACHE_MAX_AGE_SECS: i64 = 24 * 3600;
const INSTALL_SCRIPT_URL: &str = "https://odm.orynix.id/install.sh";

/// `check_latest` fetches the latest release version from GitHub API.
/// Returns the bare version string (e.g. "1.8.0") and the tarball download
/// URL. Uses `curl` (Go's pure-Go resolver had issues on Android/Termux; the
/// behavior is the same).
pub fn check_latest() -> Result<(String, String), String> {
    let out = Command::new("curl")
        .args(["-fsSL", GITHUB_API])
        .output()
        .map_err(|e| format!("GitHub API (curl): {e}"))?;
    if !out.status.success() {
        return Err(format!("GitHub API (curl): exit {}", out.status));
    }
    let release: serde_json::Value =
        serde_json::from_slice(&out.stdout).map_err(|e| format!("GitHub API: {e}"))?;
    let tag = release.get("tag_name").and_then(|v| v.as_str()).unwrap_or("").to_string();
    let ver = tag.strip_prefix('v').unwrap_or(&tag).to_string();
    if ver.is_empty() {
        return Err("GitHub API: empty tag_name".into());
    }
    Ok((ver.clone(), format!(
        "https://github.com/{}/{}/releases/download/v{}/odm_{}_{}_{}.tar.gz",
        REPO_OWNER, REPO_NAME, ver, ver, goos(), goarch()
    )))
}

/// `compare_versions` compares two semver strings (major.minor.patch).
/// Returns -1 if a < b, 0 if equal, 1 if a > b.
pub fn compare_versions(a: &str, b: &str) -> i32 {
    let av = parse_semver(a);
    let bv = parse_semver(b);
    for i in 0..3 {
        if av[i] < bv[i] {
            return -1;
        }
        if av[i] > bv[i] {
            return 1;
        }
    }
    0
}

fn parse_semver(s: &str) -> [i64; 3] {
    let s = s.strip_prefix('v').unwrap_or(s);
    let s = s.strip_prefix("odm/").unwrap_or(s);
    let mut parts = [0i64; 3];
    for (i, p) in s.splitn(3, '.').enumerate().take(3) {
        parts[i] = p.parse().unwrap_or(0);
    }
    parts
}

/// Rust's arch names map onto Go's GOARCH values.
fn goarch() -> &'static str {
    match env::consts::ARCH {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        "arm" => "arm",
        "x86" => "386",
        other => other,
    }
}

/// Rust's OS names map onto Go's GOOS values.
fn goos() -> &'static str {
    match env::consts::OS {
        "macos" => "darwin",
        other => other,
    }
}

/// `InstallMethod` describes how ODM was installed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InstallMethod {
    MethodSelf,
    MethodAUR,
    MethodManual,
}

impl fmt::Display for InstallMethod {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            InstallMethod::MethodSelf => write!(f, "self"),
            InstallMethod::MethodAUR => write!(f, "aur"),
            InstallMethod::MethodManual => write!(f, "manual"),
        }
    }
}

/// `detect_install_method` returns how the running binary was installed.
pub fn detect_install_method() -> InstallMethod {
    if is_aur() {
        return InstallMethod::MethodAUR;
    }
    let exe = env::current_exe().ok().and_then(|p| p.canonicalize().ok());
    let dir = match exe {
        Some(e) => e.parent().map(|p| p.to_path_buf()),
        None => return InstallMethod::MethodManual,
    };
    let home = env::var_os("HOME").map(PathBuf::from).unwrap_or_default();
    let self_dirs = [home.join(".local").join("bin"), PathBuf::from("/usr/local/bin")];
    for d in self_dirs {
        if dir == Some(d) {
            return InstallMethod::MethodSelf;
        }
    }
    InstallMethod::MethodManual
}

fn is_aur() -> bool {
    // Check if pacman exists first — on Android/Termux exec.LookPath SIGSYS,
    // so stat the common locations instead.
    let has_pacman = ["/usr/bin/pacman", "/usr/local/bin/pacman"].iter().any(|p| Path::new(p).exists());
    if !has_pacman {
        return false;
    }
    Command::new("pacman")
        .args(["-Qi", "odm-bin"])
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

fn has_in_path(name: &str) -> bool {
    if let Some(paths) = env::var_os("PATH") {
        for d in env::split_paths(&paths) {
            if d.join(name).exists() {
                return true;
            }
        }
    }
    false
}

/// `run` executes the self-update for the detected install method.
pub fn run(current_version: &str) -> Result<(), String> {
    let method = detect_install_method();
    let (latest_ver, _) = check_latest().map_err(|e| format!("checking for updates: {e}"))?;

    if compare_versions(current_version, &latest_ver) >= 0 {
        println!("already up to date ({current_version})");
        return Ok(());
    }

    println!("updating odm {current_version} → {latest_ver}");

    match method {
        InstallMethod::MethodAUR => run_aur().map_err(|e| e.to_string()),
        InstallMethod::MethodSelf => run_install_script().map_err(|e| e.to_string()),
        InstallMethod::MethodManual => run_manual(&latest_ver).map_err(|e| e.to_string()),
    }
}

fn run_aur() -> io::Result<()> {
    let helper = ["yay", "paru"].into_iter().find(|h| has_in_path(h));
    match helper {
        None => {
            println!("no AUR helper found (yay/paru). Install one or update manually:");
            println!("  yay -S odm-bin");
            Ok(())
        }
        Some(helper) => {
            println!("running: {helper} -S odm-bin");
            Command::new(helper).arg("-S").arg("odm-bin").status().map(|_| ())
        }
    }
}

/// `run_install_script` downloads the install.sh and runs it with
/// `--update` + `ODM_UPDATE=1`.
pub fn run_install_script() -> io::Result<()> {
    // Resolve temp dir: prefer $TMPDIR, then os.TempDir(), then
    // ~/.odm-tmp.
    let mut tmp_dir = env::var_os("TMPDIR")
        .map(PathBuf::from)
        .filter(|s| !s.as_os_str().is_empty())
        .unwrap_or_else(env::temp_dir);
    if std::fs::create_dir_all(&tmp_dir).is_err() {
        let home = env::var_os("HOME").map(PathBuf::from).unwrap_or_default();
        tmp_dir = home.join(".odm-tmp");
        let _ = std::fs::create_dir_all(&tmp_dir);
    }

    // os.CreateTemp(tmpDir, "odm-install-*.sh").
    let mut tmp_path = PathBuf::new();
    for n in 0..100u32 {
        let candidate = tmp_dir.join(format!("odm-install-{}-{}.sh", std::process::id(), n));
        let f = match OpenOptions::new().write(true).create_new(true).open(&candidate) {
            Ok(f) => f,
            Err(_) => continue,
        };
        drop(f);
        tmp_path = candidate;
        break;
    }
    if tmp_path.as_os_str().is_empty() {
        return Err(io::Error::new(ErrorKind::Other, "creating temp file"));
    }

    let result = run_install_script_inner(&tmp_path);
    let _ = std::fs::remove_file(&tmp_path);
    result
}

fn run_install_script_inner(tmp_path: &Path) -> io::Result<()> {
    println!("downloading install script...");
    let dl = Command::new("curl")
        .args(["-fsSL", "-o"])
        .arg(tmp_path)
        .arg(INSTALL_SCRIPT_URL)
        .status()
        .map_err(|e| io::Error::other(format!("downloading install script: {e}")))?;
    if !dl.success() {
        return Err(io::Error::other("downloading install script: non-zero exit"));
    }
    let sh = Command::new("bash")
        .arg(tmp_path)
        .arg("--update")
        .env("ODM_UPDATE", "1")
        .status()
        .map_err(|e| io::Error::other(format!("install script failed: {e}")))?;
    if !sh.success() {
        return Err(io::Error::other("install script failed: non-zero exit"));
    }
    Ok(())
}

fn run_manual(latest_ver: &str) -> io::Result<()> {
    println!("new version available: {latest_ver}");
    run_install_script()
}

/// `hint` returns a version-hint string if a newer release exists, or None
/// if up-to-date or the check is throttled. The result is cached for 24h.
pub fn hint(current_version: &str) -> Option<String> {
    // check cache
    if let Some(path) = hint_cache_path() {
        if let Ok(data) = std::fs::read_to_string(&path) {
            let mut parts = data.splitn(2, '\n');
            if let (Some(ts_str), Some(cached_ver)) = (parts.next(), parts.next()) {
                let ts: i64 = ts_str.parse().unwrap_or(0);
                let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or(Duration::ZERO).as_secs() as i64;
                if now - ts < HINT_CACHE_MAX_AGE_SECS {
                    return if compare_versions(current_version, cached_ver) < 0 {
                        Some(format!("→ update available: v{} (odm update)", cached_ver))
                    } else {
                        None
                    };
                }
            }
        }
    }

    // fetch latest (with short timeout)
    let Ok((latest_ver, _)) = check_latest() else {
        return None;
    };

    // write cache (best-effort)
    if let Some(cache_path) = hint_cache_path() {
        if let Some(dir) = cache_path.parent() {
            let _ = std::fs::create_dir_all(dir);
        }
        let now = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
        let _ = std::fs::write(cache_path, format!("{now}\n{latest_ver}"));
    }

    if compare_versions(current_version, &latest_ver) < 0 {
        return Some(format!("→ update available: v{} (odm update)", latest_ver));
    }
    None
}

pub fn hint_cache_path() -> Option<PathBuf> {
    let home = env::var_os("HOME").map(PathBuf::from).filter(|h| !h.as_os_str().is_empty())?;
    Some(home.join(HINT_CACHE_DIR).join(HINT_CACHE_FILE))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_compare_versions() {
        let cases: &[(&str, &str, i32)] = &[
            ("1.7.0", "1.7.0", 0),
            ("1.7.0", "1.8.0", -1),
            ("1.8.0", "1.7.0", 1),
            ("1.7.0", "1.7.1", -1),
            ("1.7.1", "1.7.0", 1),
            ("2.0.0", "1.9.9", 1),
            ("1.9.9", "2.0.0", -1),
            ("10.20.30", "10.20.31", -1),
            ("v1.7.0", "1.7.0", 0),
            ("odm/1.7.0", "1.7.0", 0),
            ("1.7.0", "v1.8.0", -1),
        ];
        for &(a, b, want) in cases {
            assert_eq!(compare_versions(a, b), want, "compare_versions({a:?}, {b:?})");
        }
    }

    #[test]
    fn test_parse_semver() {
        let cases: &[(&str, [i64; 3])] = &[
            ("1.7.0", [1, 7, 0]),
            ("v2.0.1", [2, 0, 1]),
            ("odm/1.7.0", [1, 7, 0]),
            ("10.20.30", [10, 20, 30]),
            ("0.0.0", [0, 0, 0]),
        ];
        for &(input, want) in cases {
            assert_eq!(parse_semver(input), want, "parse_semver({input:?})");
        }
    }

    #[test]
    fn test_detect_install_method() {
        // Just verify it doesn't panic and returns a valid method
        let m = detect_install_method();
        assert!(matches!(
            m,
            InstallMethod::MethodSelf | InstallMethod::MethodAUR | InstallMethod::MethodManual
        ));
    }

    #[test]
    fn test_install_method_string() {
        assert_eq!(InstallMethod::MethodSelf.to_string(), "self");
        assert_eq!(InstallMethod::MethodAUR.to_string(), "aur");
        assert_eq!(InstallMethod::MethodManual.to_string(), "manual");
    }
}
