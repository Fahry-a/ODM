//! Rust port of ODM's `internal/ratelimit` package: the global token bucket
//! limiter. --limit-rate is enforced by ONE limiter shared across every
//! active worker of every task. Workers acquire tokens in proportion to the
//! bytes they are about to read (before writing to disk), so throttling
//! happens at the data-stream level and aggregate throughput stays at the
//! configured ceiling no matter how many connections are alive.
//!
//! When the limiter is "off" (unset/`off`/`0`), `unlimited()` is true and
//! `acquire` is a no-op — zero per-byte overhead.
//!
//! Go uses `golang.org/x/time/rate`; we hand-roll an equivalent O(1) bucket.

use std::io::Read;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const MIN_ADAPTIVE_BPS: i64 = 64 * 1024;
const ADAPTIVE_COOLDOWN: Duration = Duration::from_secs(30);

#[derive(Debug, Clone)]
struct Bucket {
    limit: f64, // tokens per second (Go: rate.Limit)
    burst: u64,
    tokens: f64,
    last: Option<Instant>,
}

impl Bucket {
    fn new(limit: f64, burst: u64) -> Bucket {
        Bucket { limit, burst, tokens: burst as f64, last: None }
    }

    fn refill(&mut self, now: Instant) {
        let last = match self.last {
            Some(l) => l,
            None => {
                self.last = Some(now);
                return;
            }
        };
        let elapsed = now.duration_since(last).as_secs_f64();
        self.tokens = (self.tokens + elapsed * self.limit).min(self.burst as f64);
        self.last = Some(now);
    }
}

/// `Limiter` is the shared global bucket (one per process).
pub struct Limiter {
    inner: Mutex<Option<Bucket>>,
    bytes: AtomicI64,
    configured: AtomicI64,
    // Shared by every task/worker using this limiter.
    cooldown_until: AtomicI64, // unix nanos; 0 = no adaptive cooldown
}

impl Default for Limiter {
    fn default() -> Self {
        Limiter {
            inner: Mutex::new(None),
            bytes: AtomicI64::new(0),
            configured: AtomicI64::new(0),
            cooldown_until: AtomicI64::new(0),
        }
    }
}

fn max_int() -> i64 {
    i64::MAX
}

fn max_int64() -> i64 {
    i64::MAX
}

/// `new` — spec like "5M", "500K", "0"/"off"/"" → unlimited.
pub fn new(spec: &str) -> Result<Limiter, String> {
    let spec = spec.trim();
    if spec.is_empty() || spec.eq_ignore_ascii_case("off") || spec == "0" {
        return Ok(Limiter::default());
    }
    let bps = parse_rate(spec)?;
    if bps <= 0 {
        return Ok(Limiter::default());
    }
    if bps > max_int() {
        return Err(format!("rate {bps} exceeds platform burst capacity"));
    }
    let l = Limiter::default();
    l.bytes.store(bps, Ordering::Relaxed);
    l.configured.store(bps, Ordering::Relaxed);
    *l.inner.lock().unwrap() = Some(Bucket::new(bps as f64, bps as u64));
    Ok(l)
}

impl Limiter {
    /// `unlimited` reports whether the limiter is off (no bucket).
    pub fn unlimited(&self) -> bool {
        self.inner.lock().unwrap().is_none()
    }

    /// `back_off_signal` halves the rate after a 429; global cooldown is set.
    pub fn back_off_signal(&self) -> bool {
        let cur = self.bytes.load(Ordering::Relaxed);
        if cur <= 0 {
            return false;
        }
        let mut half = cur / 2;
        if half < MIN_ADAPTIVE_BPS {
            half = MIN_ADAPTIVE_BPS;
        }
        if half == cur {
            return false;
        }
        self.set_bytes(half);
        let until = now_unix_nanos() + ADAPTIVE_COOLDOWN.as_nanos() as i64;
        self.cooldown_until.store(until, Ordering::Relaxed);
        true
    }

    /// `throttle_ok` restores the configured rate after the global cooldown.
    pub fn throttle_ok(&self) {
        let until = self.cooldown_until.load(Ordering::Relaxed);
        if until == 0 || now_unix_nanos() < until {
            return;
        }
        if self
            .cooldown_until
            .compare_exchange(until, 0, Ordering::Relaxed, Ordering::Relaxed)
            .is_ok()
        {
            self.reset_rate();
        }
    }

    /// `reset_rate` restores the configured rate (used after throttle_ok).
    pub fn reset_rate(&self) {
        let cfg = self.configured.load(Ordering::Relaxed);
        if cfg > 0 && self.bytes.load(Ordering::Relaxed) != cfg {
            self.set_bytes(cfg);
        }
    }

    /// `set_bytes` mutates the live bucket when one exists.
    fn set_bytes(&self, bps: i64) {
        self.bytes.store(bps, Ordering::Relaxed);
        let mut guard = self.inner.lock().unwrap();
        if let Some(b) = guard.as_mut() {
            b.limit = bps as f64;
            b.burst = bps as u64;
        }
    }

    /// `set_rate` re-specifies the limiter; empty/off/0 disables.
    pub fn set_rate(&self, spec: &str) -> Result<(), String> {
        let spec = spec.trim();
        if spec.is_empty() || spec.eq_ignore_ascii_case("off") || spec == "0" {
            *self.inner.lock().unwrap() = None;
            self.bytes.store(0, Ordering::Relaxed);
            self.cooldown_until.store(0, Ordering::Relaxed);
            return Ok(());
        }
        let bps = parse_rate(spec)?;
        if bps <= 0 {
            *self.inner.lock().unwrap() = None;
            self.bytes.store(0, Ordering::Relaxed);
            self.cooldown_until.store(0, Ordering::Relaxed);
            return Ok(());
        }
        if bps > max_int() {
            return Err(format!("rate {bps} exceeds platform burst capacity"));
        }
        self.bytes.store(bps, Ordering::Relaxed);
        self.configured.store(bps, Ordering::Relaxed);
        self.cooldown_until.store(0, Ordering::Relaxed);
        let mut guard = self.inner.lock().unwrap();
        match guard.as_mut() {
            None => *guard = Some(Bucket::new(bps as f64, bps as u64)),
            Some(b) => {
                b.limit = bps as f64;
                b.burst = bps as u64;
            }
        }
        Ok(())
    }

    /// `acquire` waits until `n` tokens are available and consumes them.
    /// `n <= 0` or unlimited is a no-op.
    pub fn acquire(&self, n: i64) -> Result<(), String> {
        if n <= 0 {
            return Ok(());
        }
        loop {
            let mut guard = self.inner.lock().unwrap();
            let b = match guard.as_mut() {
                None => return Ok(()),
                Some(b) => b,
            };
            b.refill(Instant::now());
            let n = n.min(b.burst as i64);
            if b.tokens >= n as f64 {
                b.tokens -= n as f64;
                return Ok(());
            }
            let wait = Duration::from_secs_f64((n as f64 - b.tokens) / b.limit);
            drop(guard);
            std::thread::sleep(wait.max(Duration::from_millis(1)));
        }
    }

    /// `acquire_async` — async variant of `acquire` (tokio sleep loop).
    pub async fn acquire_async(&self, n: i64) -> Result<(), String> {
        if n <= 0 {
            return Ok(());
        }
        loop {
            // Compute the wait time with the lock held (b.refill + check);
            // the guard is dropped at block end — never across the sleep.
            let wait = {
                let mut guard = self.inner.lock().unwrap();
                let b = match guard.as_mut() {
                    None => return Ok(()),
                    Some(b) => b,
                };
                b.refill(Instant::now());
                let n = n.min(b.burst as i64);
                if b.tokens >= n as f64 {
                    b.tokens -= n as f64;
                    return Ok(());
                }
                Duration::from_secs_f64((n as f64 - b.tokens) / b.limit)
            };
            tokio::time::sleep(wait.max(Duration::from_millis(1))).await;
        }
    }

    /// `reader` wraps a reader to acquire tokens on every read.
    pub fn reader<'a, R: Read>(&'a self, src: R) -> RateReader<'a, R> {
        RateReader { src, l: self }
    }
}

/// `RateReader` wraps a `Read` to acquire tokens in proportion to bytes read.
pub struct RateReader<'a, R> {
    src: R,
    l: &'a Limiter,
}

impl<R: Read> Read for RateReader<'_, R> {
    fn read(&mut self, p: &mut [u8]) -> std::io::Result<usize> {
        let n = self.src.read(p)?;
        if n > 0 {
            if let Err(werr) = self.l.acquire(n as i64) {
                return Err(std::io::Error::other(werr));
            }
        }
        Ok(n)
    }
}

/// `ParseRate` — "5M/s", "500K", "1.5M", "5Kb" (lowercase-b suffix accepted).
/// Go's `%q` for user strings (printable runes literal, ZWJ/NBSP escaped).
/// Duplicated here because `config` (the higher layer) depends on this crate.
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
            _ if printable(c) => out.push(c),
            _ if (c as u32) <= 0xffff => out.push_str(&format!("\\u{:04x}", c as u32)),
            _ => out.push_str(&format!("\\U{:08x}", c as u32)),
        }
    }
    out.push('"');
    out
}

pub fn parse_rate(spec: &str) -> Result<i64, String> {
    let mut s = spec.trim();
    if s.is_empty() {
        return Err("empty rate".to_string());
    }
    if let Some(rest) = s.strip_suffix("/s").or_else(|| s.strip_suffix("/S")) {
        s = rest.trim();
    }
    let mut mult: i64 = 1;
    let mut trim_n: usize = 0;
    let u = s.to_uppercase();
    for (suffix, m, t) in [
        ("KB", 1024, 2),
        ("K", 1024, 1),
        ("MB", 1024 * 1024, 2),
        ("M", 1024 * 1024, 1),
        ("GB", 1024 * 1024 * 1024, 2),
        ("G", 1024 * 1024 * 1024, 1),
        ("TB", 1024 * 1024 * 1024 * 1024, 2),
        ("T", 1024 * 1024 * 1024 * 1024, 1),
    ] {
        if u.ends_with(suffix) {
            mult = m;
            trim_n = t;
            break;
        }
    }
    if trim_n > 0 {
        s = s[..s.len() - trim_n].trim();
    }
    // Match Go's strconv.ParseFloat error text: the ParseFloat part quotes
    // the *suffix-trimmed* value, while the `invalid rate` prefix quotes
    // the original spec.
    let q_spec = go_quote(spec);
    let q_trimmed = go_quote(s);
    let v: f64 = s
        .parse()
        .map_err(|_| format!("invalid rate {q_spec}: strconv.ParseFloat: parsing {q_trimmed}: invalid syntax"))?;
    if v.is_nan() || v.is_infinite() || v < 0.0 {
        return Err(format!("invalid rate {q_spec}"));
    }
    let max = max_int64() as f64 / mult as f64;
    if v > max {
        return Err(format!("rate {q_spec} overflows int64"));
    }
    let bps = (v * mult as f64) as i64;
    if bps > max_int() {
        return Err(format!("rate {q_spec} exceeds platform int capacity"));
    }
    Ok(bps)
}

fn now_unix_nanos() -> i64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_nanos() as i64).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    #[tokio::test]
    async fn acquire_async_consumes_burst_and_clamps_to_burst() {
        let l = new("1000").unwrap(); // burst == 1000 tokens, starts full
        // Request beyond burst: clamped to what was in the bucket.
        l.acquire_async(5000).await.unwrap();
        let tokens = l.inner.lock().unwrap().as_ref().unwrap().tokens;
        assert_eq!(tokens, 0.0);
        // Empty bucket + non-blocking zero request: returns immediately.
        l.acquire_async(0).await.unwrap();
    }

    #[tokio::test]
    async fn acquire_async_blocks_until_token_refills() {
        let l = new("1000").unwrap();
        l.acquire_async(1000).await.unwrap(); // drain burst
        let t0 = Instant::now();
        l.acquire_async(1).await.unwrap(); // must wait ~1ms (limit 1000/s)
        assert!(t0.elapsed() >= Duration::from_micros(500));
    }

    #[test]
    fn test_parse_rate() {
        let cases: &[(&str, i64)] = &[
            ("500", 500),
            ("500K", 500 * 1024),
            ("5M", 5 * 1024 * 1024),
            ("2G", 2 * 1024 * 1024 * 1024),
            ("1.5M", 1572864), // int64(1.5 * 1024 * 1024) — Go: int64(v * mult) truncation
            ("5MB/s", 5 * 1024 * 1024),
            ("5M/s", 5 * 1024 * 1024),
            ("5Kb", 5 * 1024), // lowercase-b suffix: accepted since the single-pass rewrite
        ];
        for &(input, want) in cases {
            let got = parse_rate(input).unwrap_or_else(|e| panic!("ParseRate({input:?}): {e}"));
            assert_eq!(got, want, "ParseRate({input:?})");
        }
    }

    #[test]
    fn test_parse_rate_invalid() {
        for bad in ["", "abc", "5Z"] {
            assert!(parse_rate(bad).is_err(), "want error for {bad:?}");
        }
    }

    #[test]
    fn test_unlimited_no_blocking() {
        let l = new("").unwrap();
        assert!(l.unlimited(), "empty spec must be unlimited");
        let start = Instant::now();
        l.acquire(1 << 30).unwrap();
        assert!(start.elapsed() <= Duration::from_millis(50), "unlimited acquire must not block");
    }

    #[test]
    fn test_reader_throttles() {
        let l = new("1K").unwrap();
        assert_eq!(l.bytes.load(Ordering::Relaxed), 1024);
        // First Acquire(consumes burst bytes) → near-instant.
        let start = Instant::now();
        l.acquire(1024).unwrap();
        assert!(start.elapsed() <= Duration::from_millis(50), "first acquire should be instant");
        // Second Acquire must wait ~1 second to refill.
        let start = Instant::now();
        l.acquire(1024).unwrap();
        let elapsed = start.elapsed();
        assert!(elapsed >= Duration::from_millis(600), "not throttled enough: {elapsed:?}");
        assert!(elapsed <= Duration::from_secs(2), "over-throttled: {elapsed:?}");

        // Reader wrapper honours the same bucket.
        let payload = vec![b'x'; 512]; // half of burst after drain
        let mut rr = l.reader(Cursor::new(payload));
        let mut buf = Vec::new();
        std::io::copy(&mut rr, &mut buf).unwrap();
        assert_eq!(buf.len(), 512);
    }

    #[test]
    fn test_adaptive_back_off() {
        let l = new("1M").unwrap();
        assert!(l.back_off_signal(), "first BackOffSignal should report a change");
        assert_eq!(l.bytes.load(Ordering::Relaxed), 512 * 1024);
        assert_eq!(l.configured.load(Ordering::Relaxed), 1024 * 1024, "configured drifted");
        l.reset_rate();
        assert_eq!(l.bytes.load(Ordering::Relaxed), 1024 * 1024);
        // Repeated halving floors at minAdaptiveBps and stops signalling.
        for _ in 0..20 {
            l.back_off_signal();
        }
        assert_eq!(l.bytes.load(Ordering::Relaxed), MIN_ADAPTIVE_BPS);

        // Unlimited limiter never signals.
        let un = new("").unwrap();
        assert!(!un.back_off_signal(), "unlimited limiter must not signal");

        // An explicit SetRate redefines the restore point.
        l.set_rate("2M").unwrap();
        l.back_off_signal();
        l.reset_rate();
        assert_eq!(l.bytes.load(Ordering::Relaxed), 2 * 1024 * 1024);
    }
}
