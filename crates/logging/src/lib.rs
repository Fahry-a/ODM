//! Rust port of ODM's `internal/logging` package: a tiny leveled logger.
//! It is deliberately stdlib-shaped so we don't pull a logging framework:
//! levels filter output, an optional --log file captures everything (mirrored),
//! and the engine (Task/Manager) plus the CLI funnel here.

use std::fmt;
use std::io::{self, Write};
use std::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

use storage::File;

/// Level is one of Debug/Info/Warn/Error. Ordered as in Go:
/// `Error < Warn < Info < Debug`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Level {
    Error,
    Warn,
    Info,
    Debug,
}

impl fmt::Display for Level {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Level::Debug => "DEBUG",
            Level::Info => "INFO",
            Level::Warn => "WARN",
            Level::Error => "ERROR",
        })
    }
}

/// `parse_level` maps the --log-level string ("debug"/"info"/"warn"/"error").
pub fn parse_level(s: &str) -> Result<Level, String> {
    match s {
        "debug" => Ok(Level::Debug),
        "info" => Ok(Level::Info),
        "warn" => Ok(Level::Warn),
        "error" => Ok(Level::Error),
        _ => Err(format!("invalid log level {s:?}")),
    }
}

/// `Logger` filters messages by level and writes to stderr (+ an optional
/// mirror file when --log is set). It is safe for concurrent use.
pub struct Logger {
    level: Level,
    out: Mutex<Box<dyn Write + Send + Sync>>,
    file: Option<File>,
}

/// `new` builds a Logger at the given level. If log_file is non-empty, it is
/// opened append-only and receives a copy of every emitted line.
pub fn new(level: Level, log_file: &str) -> io::Result<Logger> {
    let mut file = None;
    if !log_file.is_empty() {
        let p = std::path::Path::new(log_file);
        let dir = p
            .parent()
            .map(|d| d.to_string_lossy().into_owned())
            .filter(|d| !d.is_empty())
            .unwrap_or_default();
        let name = p
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(|| log_file.to_string());
        let f = File::open_for_write(&dir, &name, 0)
            .map_err(|e| io::Error::other(format!("--log {log_file:?}: {e}")))?;
        file = Some(f);
    }
    Ok(Logger { level, out: Mutex::new(Box::new(io::stderr())), file })
}

impl Logger {
    /// `set_output` redirects the primary stream (default stderr). The CLI
    /// wires this to the UI renderer's frame-safe printer during an
    /// interactive run so engine logs can't corrupt the live progress frame;
    /// the --log file mirror is unaffected. Safe for concurrent use.
    pub fn set_output(&self, out: Option<Box<dyn Write + Send + Sync>>) {
        *self.out.lock().unwrap() = out.unwrap_or_else(|| Box::new(io::sink()));
    }

    /// `logf` fans a formatted message to the primary writer (and the file
    /// mirror) if the level passes the threshold.
    fn logf(&self, lvl: Level, msg: fmt::Arguments) {
        if lvl > self.level {
            return;
        }
        let line = format!("{}{}: {}\n", timestamp_header(), lvl, msg);
        // Hold the lock across both writers: File::append uses len()+WriteAt,
        // so two loggers writing together would overwrite each other's line.
        let mut out = self.out.lock().unwrap();
        let _ = out.write_all(line.as_bytes());
        if let Some(f) = &self.file {
            let _ = f.append(line.as_bytes());
        }
    }

    /// Level-tagged entry points (Go Debugf/Infof/Warnf/Errorf).
    pub fn debug(&self, msg: fmt::Arguments) {
        self.logf(Level::Debug, msg);
    }
    pub fn info(&self, msg: fmt::Arguments) {
        self.logf(Level::Info, msg);
    }
    pub fn warn(&self, msg: fmt::Arguments) {
        self.logf(Level::Warn, msg);
    }
    pub fn error(&self, msg: fmt::Arguments) {
        self.logf(Level::Error, msg);
    }

    /// `task_log_fn` adapts a Logger to the download.LogFn signature
    /// (level, format, args...) so the engine can plug straight in.
    pub fn task_log_fn(&self) -> impl Fn(&str, fmt::Arguments) + Send + Sync + '_ {
        move |level: &str, msg: fmt::Arguments| {
            let lvl = match level {
                "debug" => Level::Debug,
                "warn" => Level::Warn,
                "error" => Level::Error,
                _ => Level::Info,
            };
            self.logf(lvl, msg);
        }
    }

    /// `close` releases the mirror file handle, if any. Idempotent: a second
    /// Close is a no-op, matching the engine's defensive close-on-shutdown
    /// pattern.
    pub fn close(&mut self) -> io::Result<()> {
        match self.file.take() {
            Some(mut f) => f.close(),
            None => Ok(()),
        }
    }
}

/// Go-log-header equivalent: `2026/11/20 17:13:55 ` (LstdFlags: date, space,
/// time, space), in the process's local timezone like Go's standard logger.
fn timestamp_header() -> String {
    let secs = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    format_log_timestamp(secs as i64, local_utc_offset())
}

/// Local UTC offset from the C library (`localtime_r`), matching Go's time.Local.
fn local_utc_offset() -> i32 {
    #[cfg(unix)]
    {
        let t = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs() as _)
            .unwrap_or(0);
        let mut tm = std::mem::MaybeUninit::<libc::tm>::zeroed();
        unsafe {
            libc::localtime_r(&t, tm.as_mut_ptr());
            tm.assume_init().tm_gmtoff as i32
        }
    }
    #[cfg(not(unix))]
    {
        0
    }
}

/// Format a unix timestamp like Go's `log.LstdFlags` ("2006/01/02 15:04:05 ").
/// Pure integer math (Howard Hinnant's civil-from-days), no chrono dependency.
fn format_log_timestamp(secs: i64, offset: i32) -> String {
    let local = secs + offset as i64;
    let days = local.div_euclid(86400);
    let rem = local.rem_euclid(86400) as u64;
    let z = days + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = (z - era * 146097) as u64; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365; // [0, 399]
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let d = doy - (153 * mp + 2) / 5 + 1; // [1, 31]
    let m = if mp < 10 { mp + 3 } else { mp - 9 }; // [1, 12]
    let year = if m <= 2 { y + 1 } else { y };
    format!(
        "{:04}/{:02}/{:02} {:02}:{:02}:{:02} ",
        year,
        m,
        d,
        rem / 3600,
        (rem / 60) % 60,
        rem % 60
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    #[test]
    fn test_parse_level() {
        let cases: &[(&str, Level)] = &[
            ("debug", Level::Debug),
            ("info", Level::Info),
            ("warn", Level::Warn),
            ("error", Level::Error),
        ];
        for &(in_, want) in cases {
            match parse_level(in_) {
                Ok(got) => assert_eq!(got, want, "parse_level({in_:?})"),
                Err(e) => panic!("parse_level({in_:?}): {e}"),
            }
        }
        for bad in ["", "trace", "verbose", "INFO"] {
            let err = parse_level(bad).expect_err("want error");
            assert!(err.contains("invalid log level"), "unexpected error: {err}");
        }
    }

    #[test]
    fn log_timestamp_applies_local_utc_offset() {
        // Go's log.LstdFlags uses local time; 1970-01-01 00:00 UTC in WIB (+7)
        // is 07:00 the same day, and in EST (-5) is 19:00 the previous day.
        assert_eq!(format_log_timestamp(0, 7 * 3600), "1970/01/01 07:00:00 ");
        assert_eq!(format_log_timestamp(0, -5 * 3600), "1969/12/31 19:00:00 ");
    }

    #[test]
    fn test_level_string() {
        assert_eq!(Level::Debug.to_string(), "DEBUG");
        assert_eq!(Level::Info.to_string(), "INFO");
        assert_eq!(Level::Warn.to_string(), "WARN");
        assert_eq!(Level::Error.to_string(), "ERROR");
    }

    struct BufWriter(Arc<Mutex<Vec<u8>>>);
    impl Write for BufWriter {
        fn write(&mut self, b: &[u8]) -> io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    fn capture_logger(level: Level) -> (Logger, Arc<Mutex<Vec<u8>>>) {
        let buf = Arc::new(Mutex::new(Vec::new()));
        let l = Logger {
            level,
            out: Mutex::new(Box::new(BufWriter(buf.clone()))),
            file: None,
        };
        (l, buf)
    }

    fn contains(buf: &Arc<Mutex<Vec<u8>>>, want: &str) -> bool {
        String::from_utf8_lossy(&buf.lock().unwrap()).contains(want)
    }

    #[test]
    fn test_logger_level_filtering() {
        let cases: &[(Level, &[&str], &[&str])] = &[
            (Level::Error, &["ERROR:"], &["WARN:", "INFO:", "DEBUG:"]),
            (Level::Warn, &["ERROR:", "WARN:"], &["INFO:", "DEBUG:"]),
            (Level::Info, &["ERROR:", "WARN:", "INFO:"], &["DEBUG:"]),
            (Level::Debug, &["ERROR:", "WARN:", "INFO:", "DEBUG:"], &[]),
        ];
        for &(level, want, drop) in cases {
            let (l, buf) = capture_logger(level);
            l.debug(format_args!("d={}", 1));
            l.info(format_args!("i={}", 2));
            l.warn(format_args!("w={}", 3));
            l.error(format_args!("e={}", 4));
            for w in want {
                assert!(contains(&buf, w), "level {level:?}: missing expected tag {w:?}");
            }
            for d in drop {
                assert!(!contains(&buf, d), "level {level:?}: tag {d:?} should be filtered");
            }
        }
    }

    #[test]
    fn test_logger_format_passthrough() {
        let (l, buf) = capture_logger(Level::Info);
        l.info(format_args!("hello {} #{}", "world", 42));
        assert!(contains(&buf, "INFO: hello world #42"), "unexpected output");
    }

    #[test]
    fn test_logger_file_mirror() {
        let dir = std::env::temp_dir().join(format!("odm-logging-mirror-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let log_path = dir.join("odm.log");
        let mut l = new(Level::Info, log_path.to_str().unwrap()).unwrap();
        l.set_output(Some(Box::new(io::sink())));

        l.info(format_args!("alpha"));
        l.warn(format_args!("beta"));
        l.error(format_args!("gamma"));
        l.close().unwrap();

        let body = std::fs::read_to_string(&log_path).unwrap();
        for want in ["INFO: alpha", "WARN: beta", "ERROR: gamma"] {
            assert!(body.contains(want), "missing {want:?} in file mirror:\n{body}");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn test_logger_new_bad_path() {
        let err = match new(Level::Info, "/nonexistent-dir/odm.log") {
            Err(e) => e,
            Ok(_) => panic!("want error for bad log path"),
        };
        assert!(err.to_string().contains("--log"), "error should mention --log: {err}");
    }

    #[test]
    fn test_logger_new_without_file() {
        let mut l = new(Level::Info, "").unwrap();
        assert!(l.file.is_none());
        l.close().unwrap();
    }

    #[test]
    fn test_logger_close_idempotent() {
        let dir = std::env::temp_dir().join(format!("odm-logging-close-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let mut l = new(Level::Info, dir.join("x.log").to_str().unwrap()).unwrap();
        l.close().unwrap();
        l.close().unwrap();
    }

    #[test]
    fn test_logger_task_log_fn_level_mapping() {
        let (l, buf) = capture_logger(Level::Warn);
        let f = l.task_log_fn();
        for lvl in ["debug", "info", "warn", "error", "unknown"] {
            f(lvl, format_args!("msg-{lvl}"));
        }
        let guard = buf.lock().unwrap();
        let out = String::from_utf8_lossy(&guard).to_string();
        drop(guard);
        for keep in ["WARN: msg-warn", "ERROR: msg-error"] {
            assert!(out.contains(keep), "missing {keep:?} in {out:?}");
        }
        for drop_ in ["DEBUG: msg-debug", "INFO: msg-info", "INFO: msg-unknown"] {
            assert!(!out.contains(drop_), "level warn must filter {drop_:?} in {out:?}");
        }
    }
}
