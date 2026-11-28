//! Rust port of ODM's `internal/config` package: the option model, the
//! `key = value` config-file parser, and the merge that implements the source
//! priority: CLI args > ~/.config/odm/config.conf > /etc/odm/config.conf >
//! defaults.
//!
//! Porting notes:
//! - Go `*pflag.FlagSet`/`BindFlags`/`CaptureChanged` → `setup()` via clap
//!   `Command::new("odm")` with the same flags/shorthands/aliases and value
//!   sources recorded as `changed` (pflag `Changed` analog:
//!   `clap::parser::ValueSource::CommandLine`).
//! - `map[string]string` → `std::collections::HashMap<String, String>`.
//! - `os/exec` → n/a. `time` → `std::time`.
//! - Go's `-sf` 2-char token (unbindable in pflag) → `normalize_args`.

use std::collections::{BTreeSet, HashMap};
use std::io::{self, BufRead, BufReader, Read};
use std::path::PathBuf;

use clap::parser::ValueSource;
use clap::{Arg, Command};
use version::VERSION;

// --- Defaults mirrors the documented default column. ---

pub const DEFAULT_CONNECTIONS: i64 = 5;
pub const DEFAULT_MAX_CONN: i64 = 32;
pub const DEFAULT_MAX_REDIRECT: i64 = 5;
pub const DEFAULT_RETRY: i64 = 3;
pub const DEFAULT_RETRY_WAIT: i64 = 2;
pub const DEFAULT_TIMEOUT: i64 = 30;
pub const DEFAULT_RPC_PORT: i64 = 6900;

// Engine profile defaults (aria2c-compatible values).
pub const DEFAULT_PROFILE: &str = "odm";
pub const DEFAULT_SPLIT: i64 = 5;
pub const DEFAULT_MIN_SPLIT_SIZE: &str = "20M";
pub const DEFAULT_MAX_CONN_PER_SERVER: i64 = 1;
pub const DEFAULT_LOG_LEVEL: &str = "info";
pub const DEFAULT_CONFIG_PATH: &str = "/etc/odm/config.conf";
pub const USER_CONFIG_REL_PATH: &str = "odm/config.conf"; // under $XDG_CONFIG_HOME / ~/.config

/// `Options` is the full set of tunables.
#[derive(Debug, Clone)]
pub struct Options {
    // Positional / derived.
    pub urls: Vec<String>, // resolved from positional args + -i input file (post-merge)
    pub out_file: String,  // --output/-o (single-file only); "" = derive from URL
    pub dir: String,       // --dir/-d
    pub input_file: String, // --input-file/-i (path to a URL list)

    // Balancer inputs.
    pub connections: i64,  // -c
    pub max_connection: i64, // --max-connections (ceiling; >32 warned)
    pub split_file: i64,   // --split-file/-sf (0 = unset ⇒ Mode B)

    // Retry / network.
    pub max_redirect: i64,
    pub retry: i64,
    pub retry_wait: i64, // seconds
    pub timeout: i64,    // seconds

    // HTTP identity / headers.
    pub user_agent: String,  // --user-agent
    pub headers: Vec<String>, // --header/-H (repeatable: "Key: value")
    pub load_cookie: String,  // --load-cookies <file> (Netscape cookie file)
    pub referer: String,      // --referer
    pub proxy: String,        // --proxy (http/https/socks5)

    // TLS / integrity.
    pub check_certificate: bool,
    pub checksum: String,    // "algo:hash"
    pub checksum_url: String, // sidecar URL carrying the digest
    pub mirrors: Vec<String>, // --mirror (repeatable)
    pub session_log: String,  // --session-log
    pub limit_rate: String,   // --limit-rate "5M"
    pub task_limit_rate: String, // --limit-rate-per-task
    pub chunk_size: String,   // --chunk-size

    // Engine profile.
    pub profile: String,
    pub split: i64,
    pub min_split_size: String,
    pub max_conn_per_server: i64,

    // Behaviour.
    pub yes: bool,
    pub quiet: bool,
    pub dry_run: bool,
    pub auto_rename: bool,
    pub skip_existing: bool,
    pub continue_: bool, // --continue/-x

    // Paths / logging.
    pub config_file: String,
    pub log_file: String,
    pub log_level: String,

    // RPC.
    pub rpc: bool,
    pub rpc_port: i64,
    pub rpc_listen_all: bool,
    pub rpc_secret: String,
    pub rpc_tls_cert: String,
    pub rpc_tls_key: String,

    // Flags explicitly set on the CLI (pflag `Changed` analog).
    pub changed: BTreeSet<String>,
}

/// `DefaultPtr` returns a freshly-defaulted Options (the Go name).
pub fn default_ptr() -> Options {
    Options {
        urls: Vec::new(),
        out_file: String::new(),
        dir: String::new(),
        input_file: String::new(),
        connections: DEFAULT_CONNECTIONS,
        max_connection: DEFAULT_MAX_CONN,
        split_file: 0,
        max_redirect: DEFAULT_MAX_REDIRECT,
        retry: DEFAULT_RETRY,
        retry_wait: DEFAULT_RETRY_WAIT,
        timeout: DEFAULT_TIMEOUT,
        user_agent: VERSION.to_string(),
        headers: Vec::new(),
        load_cookie: String::new(),
        referer: String::new(),
        proxy: String::new(),
        check_certificate: true,
        checksum: String::new(),
        checksum_url: String::new(),
        mirrors: Vec::new(),
        session_log: String::new(),
        limit_rate: String::new(),
        task_limit_rate: String::new(),
        chunk_size: "4M".to_string(),
        profile: DEFAULT_PROFILE.to_string(),
        split: DEFAULT_SPLIT,
        min_split_size: DEFAULT_MIN_SPLIT_SIZE.to_string(),
        max_conn_per_server: DEFAULT_MAX_CONN_PER_SERVER,
        yes: false,
        quiet: false,
        dry_run: false,
        auto_rename: false,
        skip_existing: false,
        continue_: true,
        config_file: DEFAULT_CONFIG_PATH.to_string(),
        log_file: String::new(),
        log_level: DEFAULT_LOG_LEVEL.to_string(),
        rpc: false,
        rpc_port: DEFAULT_RPC_PORT,
        rpc_listen_all: false,
        rpc_secret: String::new(),
        rpc_tls_cert: String::new(),
        rpc_tls_key: String::new(),
        changed: BTreeSet::new(),
    }
}

impl Options {
    /// `is_set` reports whether the named flag was explicitly set on the CLI.
    pub fn is_set(&self, name: &str) -> bool {
        self.changed.contains(name)
    }

    /// `changed_flag` records that the named CLI flag was explicitly provided.
    pub fn changed_flag(&mut self, name: &str) {
        self.changed.insert(name.to_string());
    }
}

// --- Config-file parsing ---

/// Go's `strconv.Quote` / `%q` for user-supplied strings: printable runes stay
/// literal, format/non-breaking/zero-width characters get `\uXXXX`.
pub fn go_quote(s: &str) -> String {
    fn printable(c: char) -> bool {
        if c == ' ' {
            return true;
        }
        if c.is_control() || c.is_whitespace() {
            return false;
        }
        let u = c as u32;
        !(0x200b..=0x200f).contains(&u)
            && !(0x202a..=0x202e).contains(&u)
            && !(0x2060..=0x206f).contains(&u)
            && u != 0xfeff
    }
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            '\r' => out.push_str("\\r"),
            _ if printable(c) => out.push(c),
            _ if (c as u32) <= 0xffff => out.push_str(&format!("\\u{:04x}", c as u32)),
            _ => out.push_str(&format!("\\U{:08x}", c as u32)),
        }
    }
    out.push('"');
    out
}

/// Go's `os.Open` error text (lowercase, no `(os error N)` suffix).
pub fn io_error_text(e: &io::Error) -> String {
    match e.kind() {
        io::ErrorKind::NotFound => "no such file or directory".to_string(),
        io::ErrorKind::PermissionDenied => "permission denied".to_string(),
        io::ErrorKind::AlreadyExists => "file exists".to_string(),
        _ => e.to_string(),
    }
}

/// `parse_file` reads a `key = value` config file, returning the parsed keys.
pub fn parse_file(path: &str) -> Result<HashMap<String, String>, String> {
    let f = std::fs::File::open(path).map_err(|e| format!("open {path}: {}", io_error_text(&e)))?;
    parse(f)
}

/// `parse_file_opt` — like parse_file, but maps "missing file" to Ok(None).
pub fn parse_file_opt(path: &str) -> Result<Option<HashMap<String, String>>, String> {
    match std::fs::File::open(path) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(format!("open {path}: {}", io_error_text(&e))),
        Ok(f) => parse(f).map(Some).map_err(|e| format!("parse {path:?}: {e}")),
    }
}

/// `parse` parses config text from a reader.
pub fn parse<R: Read>(r: R) -> Result<HashMap<String, String>, String> {
    let mut out = HashMap::new();
    let mut line_no = 0usize;
    for raw in BufReader::new(r).lines() {
        line_no += 1;
        let raw = raw.map_err(|e| format!("read: {e}"))?;
        let line = raw.trim().to_string();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let (key, val) =
            line.split_once('=').ok_or_else(|| format!("config {line_no}: invalid line (no '='): {raw:?}"))?;
        out.insert(key.trim().to_string(), strip_inline_comment(val).trim().to_string());
    }
    Ok(out)
}

/// `strip_inline_comment` removes a trailing " #..." (space + hash).
fn strip_inline_comment(val: &str) -> &str {
    match val.split_once(" #") {
        Some((before, _)) => before,
        None => val,
    }
}

// --- Merging ---

impl Options {
    /// `apply` merges parsed config-file keys onto `self`. A key the user
    /// explicitly set on the CLI is NOT overwritten (honours
    /// CLI > user config > system config > defaults).
    pub fn apply(&mut self, kv: &HashMap<String, String>) -> Result<(), String> {
        for (k, v) in kv {
            if self.is_set(k) {
                continue; // CLI explicitly set this flag; config must not override it.
            }
            self.set_from_key(k, v)?;
        }
        Ok(())
    }

    /// `set_from_key` applies a single key=value. Keys match the CLI
    /// long-flag names (without `--`).
    pub fn set_from_key(&mut self, key: &str, val: &str) -> Result<(), String> {
        match key {
            "connections" => set_int(val, &mut self.connections, key)?,
            "max-connections" => set_int(val, &mut self.max_connection, key)?,
            "split-file" => {
                if val.is_empty() {
                    self.split_file = 0; // unset ⇒ Mode B
                } else {
                    set_int(val, &mut self.split_file, key)?;
                }
            }
            "dir" => self.dir = val.into(),
            "output" => self.out_file = val.into(),
            "max-redirect" => set_int(val, &mut self.max_redirect, key)?,
            "retry" => set_int(val, &mut self.retry, key)?,
            "retry-wait" => set_int(val, &mut self.retry_wait, key)?,
            "timeout" => set_int(val, &mut self.timeout, key)?,
            "user-agent" => self.user_agent = val.into(),
            "load-cookies" => self.load_cookie = val.into(),
            "referer" => self.referer = val.into(),
            "proxy" => self.proxy = val.into(),
            "check-certificate" => set_bool(val, &mut self.check_certificate, key)?,
            "continue" => set_bool(val, &mut self.continue_, key)?,
            "quiet" => set_bool(val, &mut self.quiet, key)?,
            "dry-run" => set_bool(val, &mut self.dry_run, key)?,
            "auto-rename" => set_bool(val, &mut self.auto_rename, key)?,
            "skip-existing" => set_bool(val, &mut self.skip_existing, key)?,
            "checksum" => self.checksum = val.into(),
            "checksum-url" => self.checksum_url = val.into(),
            "mirror" => self.mirrors.push(val.into()),
            "session-log" => self.session_log = val.into(),
            "limit-rate" => self.limit_rate = val.into(),
            "limit-rate-per-task" => self.task_limit_rate = val.into(),
            "chunk-size" => self.chunk_size = val.into(),
            "profile" => self.profile = val.into(),
            "split" => set_int(val, &mut self.split, key)?,
            "min-split-size" => self.min_split_size = val.into(),
            "max-connection-per-server" => set_int(val, &mut self.max_conn_per_server, key)?,
            "rpc" => set_bool(val, &mut self.rpc, key)?,
            "rpc-listen-port" => set_int(val, &mut self.rpc_port, key)?,
            "rpc-listen-all" => set_bool(val, &mut self.rpc_listen_all, key)?,
            "rpc-secret" => self.rpc_secret = val.into(),
            "rpc-tls-cert" => self.rpc_tls_cert = val.into(),
            "rpc-tls-key" => self.rpc_tls_key = val.into(),
            "log" => self.log_file = val.into(),
            "log-level" => self.log_level = val.into(),
            // Unknown key: skip (forward-compatible), but ignore safely.
            _ => {}
        }
        Ok(())
    }

    /// `validate` enforces the basic invariants before any network activity.
    pub fn validate(&self) -> Result<(), String> {
        let mut errs: Vec<String> = Vec::new();
        if self.connections < 1 {
            errs.push("connection budget (-c) must be at least 1".into());
        }
        if self.split_file < 0 {
            errs.push("--split-file must not be negative".into());
        }
        if self.split_file != 0 && self.split_file > self.connections {
            errs.push("split-file (-sf) cannot be greater than the total connection budget (-c)".into());
        }
        if self.auto_rename && self.skip_existing {
            errs.push("--auto-rename and --skip-existing are mutually exclusive (pick one collision policy)".into());
        }
        if !(1..=65535).contains(&self.rpc_port) {
            errs.push("--rpc-listen-port out of range (1-65535)".into());
        }
        if (self.rpc_tls_cert == "") != (self.rpc_tls_key == "") {
            errs.push("--rpc-tls-cert and --rpc-tls-key must be provided together".into());
        }
        match self.log_level.as_str() {
            "debug" | "info" | "warn" | "error" => {}
            _ => errs.push(format!("invalid --log-level {} (want debug|info|warn|error)", go_quote(&self.log_level))),
        }
        if !self.checksum.is_empty() {
            match self.checksum.split_once(':') {
                Some((algo, hash)) if !hash.is_empty() => {
                    match algo.to_lowercase().as_str() {
                        "md5" | "sha1" | "sha256" => {}
                        _ => errs.push(format!("--checksum unsupported algorithm {} (md5/sha1/sha256)", go_quote(algo))),
                    }
                }
                _ => errs.push(format!("--checksum format is algo:hash (e.g. sha256:<hex>)")),
            }
        }
        match self.profile.as_str() {
            "odm" | "aria2c" | "both" | "smart" => {}
            _ => errs.push(format!("invalid --profile {} (want odm|aria2c|both|smart)", go_quote(&self.profile))),
        }
        if self.profile != "odm" && self.profile != "smart" && self.split_file != 0 {
            errs.push(format!(
                "--split-file is only valid with the odm or smart profile (got --profile {})",
                self.profile
            ));
        }
        if self.profile == "both" && self.chunk_size != "4M" {
            errs.push("--chunk-size is not supported with --profile both (fixed 50/50 split)".into());
        }
        if self.split < 0 || self.max_conn_per_server < 1 {
            errs.push("--split and --max-connection-per-server must be ≥ 1".into());
        }
        if self.retry < 0 || self.retry_wait < 0 {
            errs.push("--retry and --retry-wait must be ≥ 0".into());
        }
        if self.profile == "odm"
            && (self.split != DEFAULT_SPLIT
                || self.min_split_size != DEFAULT_MIN_SPLIT_SIZE
                || self.max_conn_per_server != DEFAULT_MAX_CONN_PER_SERVER)
        {
            errs.push("--split / --min-split-size / --max-connection-per-server are only valid with --profile aria2c|both|smart".into());
        }
        if self.rpc_listen_all
            && self.rpc_secret.is_empty()
            && self.rpc_tls_cert.is_empty()
            && self.rpc_tls_key.is_empty()
        {
            errs.push("--rpc-listen-all requires --rpc-secret or TLS (--rpc-tls-cert/--rpc-tls-key) for authentication".into());
        }
        if errs.is_empty() {
            Ok(())
        } else {
            Err(errs[0].clone()) // first error wins, like Go's early return
        }
    }
}

fn set_int(s: &str, dst: &mut i64, key: &str) -> Result<(), String> {
    match s.trim().parse::<i64>() {
        Ok(v) => {
            *dst = v;
            Ok(())
        }
        Err(_) => Err(format!("{key}: not an integer ({s:?})")),
    }
}

fn set_bool(s: &str, dst: &mut bool, key: &str) -> Result<(), String> {
    match s.trim().to_lowercase().as_str() {
        "true" | "yes" | "1" | "on" => {
            *dst = true;
            Ok(())
        }
        "false" | "no" | "0" | "off" | "" => {
            *dst = false;
            Ok(())
        }
        _ => Err(format!("{key}: not a boolean ({s:?})")),
    }
}

// --- CLI parsing (pflag → clap) ---

/// `normalize_args` rewrites the documented 2-char shortcut flags that pflag
/// cannot bind as true shorthands (only single-ASCII-char shorthands are
/// legal). Practically: `-sf` → `--split-file`.
pub fn normalize_args(args: &[String]) -> Vec<String> {
    // Numeric flags accept negative values in pflag but clap treats a bare
    // "-7" as another flag. Fold `-c -7` into `-c=-7` before parsing so
    // validation at the boundary reports the real problem (budget < 1).
    const NUMERIC: &[&str] = &[
        "-c", "--connections", "-m", "--max-connections", "--split-file", "-sf", "-n", "--max-redirect",
        "-r", "--retry", "-w", "--retry-wait", "-t", "--timeout", "-s", "--chunk-size",
        "--split", "--min-split-size", "--max-connection-per-server", "--rpc-listen-port",
    ];
    let mut out = Vec::with_capacity(args.len());
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        if NUMERIC.contains(&a.as_str())
            && i + 1 < args.len()
            && args[i + 1].starts_with('-')
            && args[i + 1][1..].chars().all(|c| c.is_ascii_digit())
        {
            // `-sf` is a documented 2-char shorthand rewritten to the long
            // form; folding it straight to `-sf=-7` would make clap read it
            // as `-s f=-7` (chunk-size).
            let flag = if a == "-sf" { "--split-file" } else { a.as_str() };
            out.push(format!("{flag}={}", args[i + 1]));
            i += 2;
            continue;
        }
        match a.as_str() {
            "-sf" => out.push("--split-file".to_string()),
            _ => {
                if let Some(rest) = a.strip_prefix("-sf=") {
                    out.push(format!("--split-file={rest}"));
                } else {
                    out.push(a.clone());
                }
            }
        }
        i += 1;
    }
    out
}

/// `setup` is the CLI bootstrap wired into cmd/odm/main.go. It owns the
/// ordering: parse flags (clap), then layer the config-file stack onto the
/// same Options (CLI values already written), then resolve URLs + validate.
///
/// Returns the populated Options.
/// pflag reports out-of-range/non-integer numeric values with one stable line;
/// clap's default error is multi-line and mentions its own parser. Check at
/// the boundary so both front doors print the same text.
fn check_numeric_ranges(args: &[String]) -> Result<(), String> {
    fn display(flag: &str) -> Option<&'static str> {
        Some(match flag {
            "-c" | "--connections" => "-c, --connections",
            "-m" | "--max-connections" => "-m, --max-connections",
            "--split-file" | "-sf" => "--split-file",
            "-n" | "--max-redirect" => "-n, --max-redirect",
            "-r" | "--retry" => "-r, --retry",
            "-w" | "--retry-wait" => "-w, --retry-wait",
            "-t" | "--timeout" => "-t, --timeout",
            "--split" => "--split",
            "--max-connection-per-server" => "--max-connection-per-server",
            "--rpc-listen-port" => "--rpc-listen-port",
            _ => return None,
        })
    }
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        let (flag, value) = if let Some((f, v)) = a.split_once('=') {
            (f.to_string(), Some(v.to_string()))
        } else if display(a).is_some() && i + 1 < args.len() {
            (a.clone(), Some(args[i + 1].clone()))
        } else {
            (a.clone(), None)
        };
        if let (Some(label), Some(v)) = (display(&flag), value) {
            let parsed = v.parse::<i128>();
            let in_range = parsed.as_ref().map_or(false, |n| *n >= i64::MIN as i128 && *n <= i64::MAX as i128);
            if !in_range {
                // pflag's strconv.ParseInt wording, kept byte-for-byte.
                let digits = v.trim().trim_start_matches(['+', '-']);
                let reason = if parsed.is_ok() || (!digits.is_empty() && digits.chars().all(|c| c.is_ascii_digit())) {
                    "value out of range"
                } else {
                    "invalid syntax"
                };
                return Err(format!(
                    "invalid argument \"{v}\" for \"{label}\" flag: strconv.ParseInt: parsing \"{v}\": {reason}"
                ));
            }
            if !a.contains('=') {
                i += 1;
            }
        }
        i += 1;
    }
    Ok(())
}

pub fn setup(argv: &[String]) -> Result<Options, String> {
    let args = normalize_args(argv);
    check_numeric_ranges(&args)?;
    let mut o = default_ptr();
    let (changed, args_after) = cli_parse(&args, &mut o)?;
    o.changed = changed;
    let config_flag_changed = args.iter().any(|a| a == "--config" || a.starts_with("--config="));
    o.load_layers(config_flag_changed, &args_after)?;
    Ok(o)
}

/// `load_layers` layers the config priority onto a fresh-defaulted Options:
/// system → user → --config, then resolve URLs + validate.
pub fn load_layers_on(o: &mut Options, config_flag_changed: bool, positional: &[String]) -> Result<(), String> {
    o.load_layers(config_flag_changed, positional)
}

impl Options {
    /// `load_layers` — called by Setup with the CLI-parsed Options; a
    /// standalone path used by tests can call via `load_layers_on`.
    fn load_layers(&mut self, config_flag_changed: bool, positional: &[String]) -> Result<(), String> {
        // 1. System config first.
        let sys_path = self.config_file.clone();
        match parse_file_opt(&sys_path) {
            Err(e) => return Err(format!("system config {sys_path:?}: {e}")),
            Ok(Some(kv)) => {
                let _ = self.apply(&kv);
            }
            Ok(None) => {}
        }

        // 2. User config overrides system. Missing is fine.
        if let Ok(up) = user_config_path() {
            if let Ok(Some(kv)) = parse_file_opt(up.to_string_lossy().as_ref()) {
                let _ = self.apply(&kv);
            }
        }

        // 3. If the CLI explicitly gave --config, that wins outright — parse
        // that file onto the already-layered base.
        if config_flag_changed && self.config_file != sys_path {
            match parse_file_opt(&self.config_file) {
                Err(e) => return Err(format!("--config {}: {e}", go_quote(&self.config_file))),
                Ok(Some(kv)) => {
                    let _ = self.apply(&kv);
                }
                Ok(None) => {}
            }
        }

        // 4. Resolve positional URLs + -i input file, then validate.
        let urls = self.resolve_urls(positional)?;
        self.urls = urls;
        self.validate()?;

        // 5. --load-cookies: parse the Netscape cookie file into a Cookie
        // header appended to Headers.
        if !self.load_cookie.is_empty() {
            let hdr = cookie_header(&self.load_cookie)?;
            self.headers.push(hdr);
        }
        Ok(())
    }
}

/// `user_config_path` resolves ~/.config/odm/config.conf (XDG_CONFIG_HOME aware).
pub fn user_config_path() -> Result<PathBuf, String> {
    let base = std::env::var_os("XDG_CONFIG_HOME")
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            let home = std::env::var_os("HOME").unwrap_or_default();
            PathBuf::from(home).join(".config")
        });
    Ok(base.join(USER_CONFIG_REL_PATH))
}

/// Render clap's unknown-flag error with pflag's wording (the rest keeps
/// clap's text until a concrete parity case needs otherwise).
fn clap_error_text(e: clap::Error) -> String {
    use clap::error::{ContextKind, ContextValue, ErrorKind};
    if e.kind() == ErrorKind::UnknownArgument {
        if let Some(ContextValue::String(arg)) = e.get(ContextKind::InvalidArg) {
            if let Some(short) = arg.strip_prefix('-').and_then(|s| s.chars().next()) {
                if !arg.starts_with("--") {
                    return format!("unknown shorthand flag: '{short}' in {arg}");
                }
            }
            return format!("unknown flag: {arg}");
        }
    }
    e.to_string()
}

/// `cli_parse` — clap parser bound to the same flags/shorthands as Go's
/// BindFlags. Returns (changed_flags, args_after_flags).
fn cli_parse(args: &[String], o: &mut Options) -> Result<(BTreeSet<String>, Vec<String>), String> {
    // Go binds these with BoolVar: `--flag` means true and `--flag=false` is
    // valid. accept an optional bool value defaulting to true when the flag is
    // bare. require_equals keeps `--flag <url>` from swallowing the URL as a
    // bool value — pflag never consumes a positional after a bool flag.
    let bool_arg = |id: &'static str, short: Option<char>, long: &'static str| {
        let mut a = Arg::new(id)
            .long(long)
            .action(clap::ArgAction::Set)
            .num_args(0..=1)
            .require_equals(true)
            .default_missing_value("true")
            .value_parser(clap::value_parser!(bool));
        if let Some(s) = short {
            a = a.short(s);
        }
        a
    };
    let cmd = Command::new("odm")
        // pflag lets a flag repeat (last value wins); clap rejects it by default.
        .args_override_self(true)
        // Strings
        .arg(Arg::new("connections").short('c').long("connections").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)).help("Total connection budget"))
        .arg(Arg::new("max-connections").short('m').long("max-connections").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)).help("Configurable ceiling for -c/-sf; exceeding it warns"))
        .arg(Arg::new("split-file").long("split-file").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)).help("Parallel connections per file during batch downloads (0 = unset)"))
        .arg(Arg::new("output").short('o').long("output").action(clap::ArgAction::Set).help("Output file name (single-file mode only)"))
        .arg(Arg::new("dir").short('d').long("dir").action(clap::ArgAction::Set).help("Destination directory"))
        .arg(Arg::new("input-file").short('i').long("input-file").action(clap::ArgAction::Set).help("Read URL list from a file (one URL per line)"))
        .arg(Arg::new("max-redirect").short('n').long("max-redirect").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)))
        .arg(Arg::new("retry").short('r').long("retry").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)))
        .arg(Arg::new("retry-wait").short('w').long("retry-wait").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)))
        .arg(Arg::new("timeout").short('t').long("timeout").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)))
        .arg(Arg::new("user-agent").short('u').long("user-agent").action(clap::ArgAction::Set))
        .arg(Arg::new("header").short('H').long("header").action(clap::ArgAction::Append).help("Add a custom HTTP header (repeatable: 'Key: value')"))
        .arg(Arg::new("load-cookies").long("load-cookies").action(clap::ArgAction::Set))
        .arg(Arg::new("referer").long("referer").action(clap::ArgAction::Set))
        .arg(Arg::new("proxy").short('p').long("proxy").action(clap::ArgAction::Set).help("Proxy (http/https/socks5)"))
        .arg(Arg::new("checksum").long("checksum").action(clap::ArgAction::Set).help("Verify checksum, format algo:hash (md5/sha1/sha256)"))
        .arg(Arg::new("checksum-url").long("checksum-url").action(clap::ArgAction::Set).help("Fetch the checksum from this sidecar URL (sha256sum/md5sum-style file)"))
        .arg(Arg::new("mirror").long("mirror").action(clap::ArgAction::Append).help("Alternate URL serving the SAME file; chunks rotate across mirrors (repeatable)"))
        .arg(Arg::new("session-log").long("session-log").action(clap::ArgAction::Set).help("Append JSONL progress/summary events to this file (for wrappers/GUIs)"))
        .arg(Arg::new("limit-rate").short('l').long("limit-rate").action(clap::ArgAction::Set).help("Global speed limit, e.g. 5M, 500K"))
        .arg(Arg::new("limit-rate-per-task").long("limit-rate-per-task").action(clap::ArgAction::Set).help("Per-task speed cap, additive to global, e.g. 2M"))
        .arg(Arg::new("chunk-size").short('s').long("chunk-size").action(clap::ArgAction::Set).help("Chunk size for the work-stealing queue, e.g. 4M"))
        .arg(Arg::new("profile").long("profile").action(clap::ArgAction::Set).help("Engine profile: odm | aria2c | both | smart"))
        .arg(Arg::new("split").long("split").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)).help("aria2c: number of segments for one file (default 5)"))
        .arg(Arg::new("min-split-size").long("min-split-size").action(clap::ArgAction::Set).help("aria2c: only split ranges ≥2x this size (default 20M)"))
        .arg(Arg::new("max-connection-per-server").long("max-connection-per-server").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)).help("aria2c: max connections to one server (-x), default 1"))
        .arg(Arg::new("config").long("config").action(clap::ArgAction::Set).help("Path to a custom config file"))
        .arg(Arg::new("log").short('L').long("log").action(clap::ArgAction::Set).help("Log file path"))
        .arg(Arg::new("log-level").long("log-level").action(clap::ArgAction::Set))
        // Bool flags
        .arg(bool_arg("yes", Some('y'), "yes"))
        .arg(bool_arg("dry-run", None, "dry-run"))
        .arg(bool_arg("auto-rename", None, "auto-rename"))
        .arg(bool_arg("skip-existing", None, "skip-existing"))
        .arg(bool_arg("quiet", Some('q'), "quiet"))
        .arg(bool_arg("continue", Some('x'), "continue"))
        .arg(bool_arg("check-certificate", None, "check-certificate"))
        .arg(bool_arg("rpc", None, "rpc"))
        .arg(bool_arg("rpc-listen-all", None, "rpc-listen-all"))
        // RPC value args
        .arg(Arg::new("rpc-listen-port").long("rpc-listen-port").action(clap::ArgAction::Set).value_parser(clap::value_parser!(i64)))
        .arg(Arg::new("rpc-secret").long("rpc-secret").action(clap::ArgAction::Set))
        .arg(Arg::new("rpc-tls-cert").long("rpc-tls-cert").action(clap::ArgAction::Set))
        .arg(Arg::new("rpc-tls-key").long("rpc-tls-key").action(clap::ArgAction::Set))
        // URLs positionals
        .arg(Arg::new("urls").num_args(0..));

    // clap expects the binary name as the first element (Go's pflag does
    // not — Setup receives args without the program name).
    let matches = cmd
        .clone()
        .try_get_matches_from(std::iter::once(String::from("odm")).chain(args.iter().cloned()))
        .map_err(clap_error_text)?;

    // Fill o from matches; record changed flags (clap CommandLine source).
    let mut changed = BTreeSet::new();
    macro_rules! set_int {
        ($id:expr, $field:ident) => {
            if matches.value_source($id) == Some(ValueSource::CommandLine) {
                o.$field = matches.get_one::<i64>($id).copied().unwrap_or(o.$field);
            }
        };
    }
    macro_rules! set_str {
        ($id:expr, $field:ident) => {
            if matches.value_source($id) == Some(ValueSource::CommandLine) {
                if let Some(v) = matches.get_one::<String>($id) {
                    o.$field = v.clone();
                }
            }
        };
    }
    macro_rules! set_strs {
        ($id:expr, $field:ident) => {
            if matches.value_source($id) == Some(ValueSource::CommandLine) {
                if let Some(vs) = matches.get_many::<String>($id) {
                    o.$field = vs.cloned().collect();
                }
            }
        };
    }
    macro_rules! mark_set {
        ($id:expr) => {
            if matches.value_source($id) == Some(ValueSource::CommandLine) {
                changed.insert($id.to_string());
            }
        };
    }
    macro_rules! set_bool {
        ($id:expr, $field:ident) => {
            if matches.value_source($id) == Some(ValueSource::CommandLine) {
                o.$field = matches.get_one::<bool>($id).copied().unwrap_or(false);
            }
        };
    }
    macro_rules! set_int_f { ($id:expr, $field:ident) => { mark_set!($id); set_int!($id, $field); } }
    macro_rules! set_str_f { ($id:expr, $field:ident) => { mark_set!($id); set_str!($id, $field); } }
    macro_rules! set_strs_f { ($id:expr, $field:ident) => { mark_set!($id); set_strs!($id, $field); } }
    macro_rules! set_bool_f { ($id:expr, $field:ident) => { mark_set!($id); set_bool!($id, $field); } }

    set_int_f!("connections", connections);
    set_int_f!("max-connections", max_connection);
    set_int_f!("split-file", split_file);
    set_str_f!("output", out_file);
    set_str_f!("dir", dir);
    set_str_f!("input-file", input_file);
    set_int_f!("max-redirect", max_redirect);
    set_int_f!("retry", retry);
    set_int_f!("retry-wait", retry_wait);
    set_int_f!("timeout", timeout);
    set_str_f!("user-agent", user_agent);
    set_strs_f!("header", headers);
    set_str_f!("load-cookies", load_cookie);
    set_str_f!("referer", referer);
    set_str_f!("proxy", proxy);
    set_str_f!("checksum", checksum);
    set_str_f!("checksum-url", checksum_url);
    set_strs_f!("mirror", mirrors);
    set_str_f!("session-log", session_log);
    set_str_f!("limit-rate", limit_rate);
    set_str_f!("limit-rate-per-task", task_limit_rate);
    set_str_f!("chunk-size", chunk_size);
    set_str_f!("profile", profile);
    set_int_f!("split", split);
    set_str_f!("min-split-size", min_split_size);
    set_int_f!("max-connection-per-server", max_conn_per_server);
    set_str_f!("config", config_file);
    set_str_f!("log", log_file);
    set_str_f!("log-level", log_level);
    set_bool_f!("yes", yes);
    set_bool_f!("dry-run", dry_run);
    set_bool_f!("auto-rename", auto_rename);
    set_bool_f!("skip-existing", skip_existing);
    set_bool_f!("quiet", quiet);
    set_bool_f!("continue", continue_);
    set_bool_f!("check-certificate", check_certificate);
    set_bool_f!("rpc", rpc);
    set_bool_f!("rpc-listen-all", rpc_listen_all);

    set_int_f!("rpc-listen-port", rpc_port);
    set_str_f!("rpc-secret", rpc_secret);
    set_str_f!("rpc-tls-cert", rpc_tls_cert);
    set_str_f!("rpc-tls-key", rpc_tls_key);

    let args_after: Vec<String> = matches
        .get_many::<String>("urls")
        .map(|vals| vals.cloned().collect())
        .unwrap_or_default();
    Ok((changed, args_after))
}

// --- Cookie file parsing ---

/// `cookie_header` reads a Netscape-format cookies.txt and returns a
/// "Cookie: k=v; k2=v2" header line.
pub fn cookie_header(path: &str) -> Result<String, String> {
    let f = std::fs::File::open(path)
        .map_err(|e| format!("load-cookies {}: open {}: {}", go_quote(path), path, io_error_text(&e)))?;
    let mut pairs: Vec<String> = Vec::new();
    for line in BufReader::new(f).lines() {
        let line = line.map_err(|e| format!("load-cookies {path:?}: {e}"))?;
        let line = line.trim().to_string();
        if line.is_empty() || line.starts_with("# ") {
            // real comments start "# "
            continue;
        }
        let line = line.strip_prefix("#HttpOnly_").map(|s| s.to_string()).unwrap_or(line);
        let fields: Vec<&str> = line.splitn(7, '\t').collect();
        if fields.len() != 7 {
            continue; // malformed row: skip, don't fail the whole file
        }
        let name = fields[5];
        let value = fields[6].trim(); // value may contain tabs before the 6th — SplitN keeps it whole
        if name.is_empty() || value.is_empty() {
            continue;
        }
        // Expired cookies are dead weight: skip.
        if let Some(first) = fields[4].split_whitespace().next() {
            if let Ok(exp) = first.parse::<i64>() {
                if exp > 0 && now_unix() >= exp {
                    continue;
                }
            }
        }
        pairs.push(format!("{name}={value}"));
    }
    if pairs.is_empty() {
        return Err(format!("load-cookies {path:?}: no cookies found"));
    }
    let mut s = String::from("Cookie: ");
    for (i, p) in pairs.iter().enumerate() {
        if i > 0 {
            s.push_str("; ");
        }
        s.push_str(p);
    }
    Ok(s)
}

fn now_unix() -> i64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0)
}

// --- URL resolution ---

impl Options {
    /// `resolve_urls` implements batch URL parsing (method on Options so
    /// metalink parsing can mutate self, like Go).
    pub fn resolve_urls(&mut self, positional: &[String]) -> Result<Vec<String>, String> {
        let mut out: Vec<String> = Vec::new();
        if !positional.is_empty() {
            out.extend(positional.iter().filter(|a| !a.is_empty()).cloned());
        }
        if !self.input_file.is_empty() && is_metalink_name(&self.input_file) {
            let input_file = self.input_file.clone();
            return parse_metalink(&input_file, self);
        }
        // -i input file: one URL per line.
        if !self.input_file.is_empty() {
            let input_file = self.input_file.clone();
            let f = std::fs::File::open(&input_file).map_err(|e| format!("input file {}: open {input_file}: {}", go_quote(&input_file), io_error_text(&e)))?;
            for line in BufReader::new(f).lines() {
                let line = line.map_err(|e| format!("input file {input_file:?}: {e}"))?;
                let line = line.trim().to_string();
                if line.is_empty() || line.starts_with('#') {
                    continue;
                }
                out.push(line);
            }
        }
        Ok(out)
    }
}

/// `is_metalink_name` reports whether path looks like a Metalink4 document.
pub fn is_metalink_name(path: &str) -> bool {
    let lower = path.to_lowercase();
    lower.ends_with(".meta4") || lower.ends_with(".metalink")
}

/// `parse_metalink` reads a Metalink4 XML file (RFC 5854): every URL of the
/// first `<file>` becomes a download target (primary first), and its strongest
/// `<hash>` becomes the --checksum spec.
pub fn parse_metalink(path: &str, o: &mut Options) -> Result<Vec<String>, String> {
    let data = std::fs::read_to_string(path).map_err(|e| format!("metalink {}: open {path}: {}", go_quote(path), io_error_text(&e)))?;
    let files = parse_metalink_xml(&data).map_err(|e| format!("metalink {path:?}: {e}"))?;
    let Some(f0) = files.first() else {
        return Err(format!("metalink {path:?}: no <file> entries"));
    };
    if f0.urls.is_empty() {
        return Err(format!("metalink {path:?}: file {:?} has no URLs", f0.name));
    }
    // Strongest hash wins: sha256 > sha1 > md5.
    let mut pick = String::new();
    for (ty, val) in &f0.hashes {
        let t = ty.to_lowercase();
        let v = val.trim().to_lowercase();
        match t.as_str() {
            "sha256" => pick = format!("sha256:{v}"),
            "sha1" => {
                if pick.is_empty() || pick.starts_with("md5:") {
                    pick = format!("sha1:{v}");
                }
            }
            "md5" => {
                if pick.is_empty() {
                    pick = format!("md5:{v}");
                }
            }
            _ => {}
        }
    }
    if !pick.is_empty() {
        o.checksum = pick;
    }
    o.mirrors.extend(f0.urls[1..].iter().cloned());
    Ok(f0.urls[..1].to_vec())
}

struct MetalinkFile {
    name: String,
    hashes: Vec<(String, String)>, // (type, val)
    urls: Vec<String>,             // URL text per <url>
}

fn attr_of(e: &quick_xml::events::BytesStart, name: &str) -> String {
    for a in e.attributes().flatten() {
        if a.key.as_ref() == name.as_bytes() {
            return String::from_utf8_lossy(a.value.as_ref()).into_owned();
        }
    }
    String::new()
}

/// Minimal reader for the Metalink4 schema used by ODM: `<file name><size
/// hashes><url location/val>`.
fn parse_metalink_xml(data: &str) -> Result<Vec<MetalinkFile>, String> {
    let mut files: Vec<MetalinkFile> = Vec::new();
    let mut cur: Option<MetalinkFile> = None;
    #[derive(PartialEq, Clone, Copy)]
    enum Mode {
        None,
        FileName,
        HashVal,
        UrlVal,
    }
    let mut mode = Mode::None;
    let mut reader = quick_xml::Reader::from_str(data);
    reader.config_mut().trim_text(true);
    let mut buf = Vec::new();
    loop {
        match reader.read_event_into(&mut buf) {
            Ok(quick_xml::events::Event::Start(e)) => match e.name().as_ref() {
                b"file" => {
                    cur = Some(MetalinkFile { name: String::new(), hashes: Vec::new(), urls: Vec::new() });
                }
                b"hash" => {
                    if let Some(ce) = cur.as_mut() {
                        let _ = attr_of(&e, "type"); // type attr is informational here
                        ce.hashes.push((attr_of(&e, "type"), String::new()));
                        mode = Mode::HashVal;
                    }
                }
                b"url" => {
                    if let Some(ce) = cur.as_mut() {
                        ce.urls.push(String::new());
                        mode = Mode::UrlVal;
                    }
                }
                b"name" => {
                    if cur.is_some() {
                        mode = Mode::FileName;
                    }
                }
                _ => {}
            },
            Ok(quick_xml::events::Event::Text(t)) => {
                let s = String::from_utf8_lossy(t.as_ref()).into_owned();
                match mode {
                    Mode::FileName => {
                        if let Some(ce) = cur.as_mut() {
                            ce.name.push_str(&s);
                        }
                    }
                    Mode::HashVal => {
                        if let Some(ce) = cur.as_mut() {
                            if let Some(h) = ce.hashes.last_mut() {
                                h.1.push_str(&s);
                            }
                        }
                    }
                    Mode::UrlVal => {
                        if let Some(ce) = cur.as_mut() {
                            if let Some(u) = ce.urls.last_mut() {
                                u.push_str(&s);
                            }
                        }
                    }
                    Mode::None => {}
                }
            }
            Ok(quick_xml::events::Event::End(e)) => match e.name().as_ref() {
                b"hash" | b"url" | b"name" => mode = Mode::None,
                b"file" => {
                    if let Some(mut ce) = cur.take() {
                        // Go drops empty <url/> elements; an empty URL would
                        // otherwise become a task and fail its probe.
                        ce.urls.retain(|u| !u.trim().is_empty());
                        files.push(ce);
                    }
                    mode = Mode::None;
                }
                _ => {}
            },
            Ok(quick_xml::events::Event::Eof) => break,
            Ok(_) => {}
            Err(e) => return Err(e.to_string()),
        }
    }
    Ok(files)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn default_o() -> Options {
        default_ptr()
    }

    // --- Parse tests ---

    #[test]
    fn test_parse_comments_and_blank_lines() {
        let r = io::Cursor::new(
            "# a comment\nconnections = 8\n\n  # indented comment\ndir = /tmp/odm\n",
        );
        let kv = parse(r).unwrap();
        assert_eq!(kv["connections"], "8");
        assert_eq!(kv["dir"], "/tmp/odm");
        assert!(!kv.contains_key("# a comment"));
    }

    #[test]
    fn test_parse_malformed_line() {
        assert!(parse(io::Cursor::new("no equals here")).is_err());
    }

    #[test]
    fn test_parse_inline_comments() {
        let r = io::Cursor::new(
            "connections = 5          # total budget\nmax-redirect = 5          # redirect hops to follow\ndir = /tmp/odm            # destination\nlimit-rate = 5M           # global cap; '#' legit in values? rare here\npiece = odd#hash          # bare hash no space → preserved\n",
        );
        let kv = parse(r).unwrap();
        assert_eq!(kv["connections"], "5");
        assert_eq!(kv["max-redirect"], "5");
        assert_eq!(kv["dir"], "/tmp/odm");
        assert_eq!(kv["limit-rate"], "5M");
        // A '#' with no leading space is part of the value, not a comment.
        assert_eq!(kv["piece"], "odd#hash");
    }

    // --- Apply tests ---

    #[test]
    fn test_apply_known_keys() {
        let mut o = default_o();
        let kv: HashMap<String, String> = [
            ("connections", "12"),
            ("max-connections", "64"),
            ("split-file", "4"),
            ("dir", "/d"),
            ("check-certificate", "false"),
            ("continue", "false"),
            ("quiet", "true"),
            ("limit-rate", "1M"),
            ("log-level", "debug"),
            ("rpc", "true"),
            ("rpc-listen-port", "9999"),
            ("rpc-listen-all", "true"),
            ("rpc-secret", "s3cr3t"),
            ("rpc-tls-cert", "/etc/odm/odm.crt"),
            ("rpc-tls-key", "/etc/odm/odm.key"),
        ]
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
        o.apply(&kv).unwrap();
        assert_eq!(o.connections, 12);
        assert_eq!(o.max_connection, 64);
        assert_eq!(o.split_file, 4);
        assert_eq!(o.dir, "/d");
        assert!(!o.check_certificate);
        assert!(!o.continue_);
        assert!(o.quiet);
        assert_eq!(o.limit_rate, "1M");
        assert_eq!(o.log_level, "debug");
        assert!(o.rpc);
        assert!(o.rpc_listen_all);
        assert_eq!(o.rpc_secret, "s3cr3t");
        assert_eq!(o.rpc_port, 9999);
        assert_eq!(o.rpc_tls_cert, "/etc/odm/odm.crt");
        assert_eq!(o.rpc_tls_key, "/etc/odm/odm.key");
    }

    #[test]
    fn test_apply_split_file_empty_unsets() {
        let mut o = default_o();
        o.split_file = 5;
        o.apply(&HashMap::from([("split-file".to_string(), String::new())])).unwrap();
        assert_eq!(o.split_file, 0, "empty split-file should unset (Mode B)");
    }

    #[test]
    fn test_apply_invalid_int() {
        let mut o = default_o();
        assert!(o.apply(&HashMap::from([("connections".to_string(), "abc".to_string())])).is_err());
    }

    #[test]
    fn test_apply_invalid_bool() {
        let mut o = default_o();
        assert!(o.apply(&HashMap::from([("quiet".to_string(), "maybe".to_string())])).is_err());
    }

    #[test]
    fn test_apply_user_overrides_system() {
        let sys = HashMap::from([("connections".to_string(), "4".to_string()), ("dir".to_string(), "/sys".to_string())]);
        let user = HashMap::from([("connections".to_string(), "20".to_string())]);
        let mut o = default_o();
        o.apply(&sys).unwrap();
        o.apply(&user).unwrap();
        assert_eq!(o.connections, 20, "user must override system");
        assert_eq!(o.dir, "/sys", "ambient system value lost");
    }

    // --- Validate tests ---

    fn vcase(o: &Options) -> Result<(), String> {
        o.validate()
    }

    #[test]
    fn test_validate_table() {
        let cases: &[(&str, Box<dyn Fn(&mut Options)>, bool)] = &[
            ("defaults", Box::new(|_| {}), true),
            ("c=0", Box::new(|o| o.connections = 0), false),
            ("retry=-1", Box::new(|o| o.retry = -1), false),
            ("retry-wait=-1", Box::new(|o| o.retry_wait = -1), false),
            ("sf>c", Box::new(|o| { o.connections = 4; o.split_file = 9; }), false),
            ("bad port", Box::new(|o| o.rpc_port = 0), false),
            ("bad loglevel", Box::new(|o| o.log_level = "verbose".to_string()), false),
            ("bad checksum fmt", Box::new(|o| o.checksum = "sha256".to_string()), false),
            ("bad checksum algo", Box::new(|o| o.checksum = "crc32:deadbeef".to_string()), false),
            ("good checksum", Box::new(|o| o.checksum = format!("sha256:{}", "a".repeat(64))), true),
            ("tls both set", Box::new(|o| { o.rpc_tls_cert = "/a/b.crt".to_string(); o.rpc_tls_key = "/a/b.key".to_string(); }), true),
            ("tls cert only", Box::new(|o| o.rpc_tls_cert = "/a/b.crt".to_string()), false),
            ("tls key only", Box::new(|o| o.rpc_tls_key = "/a/b.key".to_string()), false),
            ("profile aria2c", Box::new(|o| o.profile = "aria2c".to_string()), true),
            ("profile both", Box::new(|o| o.profile = "both".to_string()), true),
            ("profile smart", Box::new(|o| o.profile = "smart".to_string()), true),
            ("profile bogus", Box::new(|o| o.profile = "banana".to_string()), false),
            ("sf + aria2c", Box::new(|o| { o.profile = "aria2c".to_string(); o.split_file = 4; }), false),
            ("sf + smart", Box::new(|o| { o.profile = "smart".to_string(); o.split_file = 4; }), true),
            ("sf + smart > c", Box::new(|o| { o.profile = "smart".to_string(); o.split_file = 9; o.connections = 4; }), false),
            ("split + aria2c ok", Box::new(|o| { o.profile = "aria2c".to_string(); o.split = 4; }), true),
            ("max-conn-per-server 0", Box::new(|o| { o.profile = "aria2c".to_string(); o.max_conn_per_server = 0; }), false),
            ("sf=-1", Box::new(|o| o.split_file = -1), false),
            ("listen-all with secret", Box::new(|o| { o.rpc_listen_all = true; o.rpc_secret = "tok".to_string(); }), true),
            ("listen-all with tls", Box::new(|o| { o.rpc_listen_all = true; o.rpc_tls_cert = "/a/b.crt".to_string(); o.rpc_tls_key = "/a/b.key".to_string(); }), true),
        ];
        for (name, mut_, ok) in cases {
            let mut o = default_o();
            mut_(&mut o);
            assert_eq!(vcase(&o).is_ok(), *ok, "case {name}");
        }
    }

    #[test]
    fn test_validate_config_file_values() {
        // Values set via apply() simulate config-file entries — validation
        // applies to final merged values.
        let cases: &[(&str, Box<dyn Fn(&mut Options)>, bool)] = &[
            ("both + chunk-size via config", Box::new(|o| { o.profile = "both".to_string(); let _ = o.apply(&HashMap::from([("chunk-size".to_string(), "8M".to_string())])); }), false),
            ("odm + split via config", Box::new(|o| { let _ = o.apply(&HashMap::from([("split".to_string(), "4".to_string())])); }), false),
            ("odm + min-split-size via config", Box::new(|o| { let _ = o.apply(&HashMap::from([("min-split-size".to_string(), "10M".to_string())])); }), false),
            ("odm + max-conn-per-server via config", Box::new(|o| { let _ = o.apply(&HashMap::from([("max-connection-per-server".to_string(), "4".to_string())])); }), false),
            ("listen-all via config no secret", Box::new(|o| { let _ = o.apply(&HashMap::from([("rpc-listen-all".to_string(), "true".to_string())])); }), false),
            ("sf=-1 via config", Box::new(|o| { let _ = o.apply(&HashMap::from([("split-file".to_string(), "-1".to_string())])); }), false),
        ];
        for (name, mut_, ok) in cases {
            let mut o = default_o();
            mut_(&mut o);
            assert_eq!(vcase(&o).is_ok(), *ok, "case {name}");
        }
    }

    // --- ResolveURLs tests ---

    #[test]
    fn test_resolve_urls_space_separated() {
        let mut o = default_o();
        let urls = o.resolve_urls(&["https://a/x?a=1,2".to_string(), "https://b/y".to_string(), "https://c/z".to_string()]).unwrap();
        assert_eq!(urls.len(), 3);
        assert_eq!(urls[0], "https://a/x?a=1,2");
    }

    #[test]
    fn test_resolve_urls_legacy_comma() {
        // The legacy comma-delimited single-arg form is gone: one arg = one URL.
        let mut o = default_o();
        let urls = o.resolve_urls(&["https://a/x,https://b/y,https://c/z".to_string()]).unwrap();
        assert_eq!(urls.len(), 1);
        assert_eq!(urls[0], "https://a/x,https://b/y,https://c/z");
    }

    #[test]
    fn test_resolve_urls_single_url_with_comma_not_split() {
        let mut o = default_o();
        let urls = o.resolve_urls(&["https://h/path?ids=1,2,3".to_string()]).unwrap();
        assert_eq!(urls.len(), 1);
    }

    #[test]
    fn test_resolve_urls_input_file() {
        let dir = std::env::temp_dir().join(format!("odm-config-resolve-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join("list.txt");
        std::fs::write(&p, "https://a\n# comment\n\nhttps://b\n").unwrap();
        let mut o = default_o();
        o.input_file = p.to_string_lossy().into_owned();
        let urls = o.resolve_urls(&[]).unwrap();
        assert_eq!(urls, vec!["https://a".to_string(), "https://b".to_string()]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    // --- NormalizeArgs tests ---

    #[test]
    fn test_normalize_args_sf() {
        let in_args = ["-c", "16", "-sf", "4", "https://x/y", "-sf=7"];
        let out = normalize_args(&in_args.iter().map(|s| s.to_string()).collect::<Vec<_>>());
        let want = ["-c", "16", "--split-file", "4", "https://x/y", "--split-file=7"];
        assert_eq!(out.len(), want.len());
        for (a, b) in out.iter().zip(want.iter()) {
            assert_eq!(a, b);
        }
    }

    #[test]
    fn test_normalize_args_does_not_mangle_bundled() {
        let out = normalize_args(&["-sfx".to_string()]);
        assert_eq!(out[0], "-sfx");
    }

    // --- Setup/CaptureChanged tests ---

    #[test]
    fn test_setup_marks_changed() {
        let o = setup(&["-c".to_string(), "9".into(), "-sf".to_string(), "3".into()]).unwrap();
        assert!(o.is_set("connections"), "connections should be marked changed");
        assert!(!o.is_set("quiet"));
        assert!(o.is_set("split-file"), "split-file (via -sf) should be marked changed");
        assert_eq!(o.connections, 9);
    }

    // --- Cookie tests ---

    #[test]
    fn test_cookie_header() {
        let dir = std::env::temp_dir().join(format!("odm-config-cookie-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("cookies.txt");
        let content = "# Netscape HTTP Cookie File\n# This is a generated file! Do not edit.\n\n.example.com\tTRUE\t/\tTRUE\t1900000000\tsession\tabc123\n#HttpOnly_.example.com\tTRUE\t/\tTRUE\t1900000000\ttoken\tXYZ\nmalformed-line-no-tabs\n.other.com\tTRUE\t/\tFALSE\t1900000000\ttheme\tdark\n";
        std::fs::write(&path, content).unwrap();
        let hdr = cookie_header(&path.to_string_lossy()).unwrap();
        for want in ["session=abc123", "token=XYZ", "theme=dark"] {
            assert!(hdr.contains(want), "cookie header missing {want:?}: {hdr}");
        }
        assert!(hdr.starts_with("Cookie: "), "header must start 'Cookie: ': {hdr:?}");
        assert!(cookie_header(&dir.join("nope.txt").to_string_lossy()).is_err(), "missing file → error");
        let empty = dir.join("empty.txt");
        std::fs::write(&empty, "# only comments\n\n").unwrap();
        let err = cookie_header(&empty.to_string_lossy()).unwrap_err();
        assert!(err.contains("no cookies"), "want 'no cookies found', got {err}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    // --- Metalink tests ---

    #[test]
    fn test_parse_metalink() {
        let dir = std::env::temp_dir().join(format!("odm-config-metalink-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("f.meta4");
        let xml_doc = format!(
            "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metalink xmlns=\"urn:ietf:params:xml:ns:metalink\">\n  <file name=\"example.iso\">\n    <size>4096</size>\n    <hash type=\"md5\">0123456789abcdef0123456789abcdef</hash>\n    <hash type=\"sha256\">{}</hash>\n    <url location=\"de\" priority=\"1\">https://mirror1.example/x.iso</url>\n    <url location=\"us\" priority=\"2\">https://mirror2.example/x.iso</url>\n  </file>\n</metalink>",
            "e".repeat(64)
        );
        std::fs::write(&path, xml_doc).unwrap();
        let mut o3 = default_o();
        let got = parse_metalink(&path.to_string_lossy(), &mut o3).unwrap();
        assert_eq!(got.len(), 1, "metalink must yield ONE target (primary)");
        assert_eq!(got[0], "https://mirror1.example/x.iso");
        assert_eq!(o3.mirrors, vec!["https://mirror2.example/x.iso"], "mirrors go to --mirror; first URL is primary");
        assert_eq!(o3.checksum, format!("sha256:{}", "e".repeat(64)), "sha256 preferred over md5");
        // Bad XML → clean error.
        let bad = dir.join("bad.meta4");
        std::fs::write(&bad, "not xml").unwrap();
        assert!(parse_metalink(&bad.to_string_lossy(), &mut default_o()).is_err());
        let _ = std::fs::remove_dir_all(&dir);
    }
    #[test]
    fn bool_flags_accept_explicit_false() {
        let mut o = default_ptr();
        let (changed, _) = super::cli_parse(
            &["--continue=false".to_string(), "--check-certificate=false".to_string()],
            &mut o,
        )
        .unwrap();
        assert!(!o.continue_);
        assert!(!o.check_certificate);
        assert!(changed.contains("continue"));
        assert!(changed.contains("check-certificate"));

        let mut o2 = default_ptr();
        super::cli_parse(&["--continue".to_string()], &mut o2).unwrap();
        assert!(o2.continue_);
    }

    #[test]
    fn bool_flag_does_not_consume_following_url() {
        let url = "http://127.0.0.1:8801/file.bin";
        let flags = [
            "-y", "-q", "-x", "--yes", "--quiet", "--continue", "--dry-run",
            "--auto-rename", "--skip-existing", "--check-certificate", "--rpc",
            "--rpc-listen-all",
        ];
        for flag in flags {
            let mut o = default_ptr();
            let (_, positional) =
                super::cli_parse(&[flag.to_string(), url.to_string()], &mut o).unwrap();
            assert_eq!(positional, vec![url.to_string()], "{flag} swallowed the URL");
        }
        let mut o = default_ptr();
        super::cli_parse(&["-y".to_string(), url.to_string()], &mut o).unwrap();
        assert!(o.yes, "bare -y must still set yes=true");
    }

}
