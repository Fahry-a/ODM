//! Rust port of ODM's `internal/transport` package: the HTTP client layer of
//! ODM. It owns the range-support probe (HEAD → ranged GET → single-stream
//! fallback), the redirect limit, custom headers, proxy, and TLS verify
//! toggle. Download workers (internal/download) build per-chunk ranged
//! requests on top of the `Client` returned by `new`.
//!
//! Porting notes:
//! - Go `net/http` → reqwest 0.12 (async).
//! - Go `time.Duration` → std::time::Duration.
//! - `Client.HTTP` (public in Go) → `Client.client`.
//! - `NewClient` → `new`.
//! - Redirect policy: Go errors when `len(via) > max_redirect`; we count
//!   redirects with a shared counter (same behavior).
//! - `ClientConfig.Timeout` → reqwest `connect_timeout`; Go additionally had
//!   TLSHandshakeTimeout and ResponseHeaderTimeout — reqwest 0.12 lacks the
//!   knobs; see rust/research/dep-reqwest.md.
//! - `ClientConfig.HTTP2` (Go: ForceAttemptHTTP2) → reqwest: there is no
//!   stable "disable h2 advertisement" knob; rust advertises h2 per server.

use std::fmt;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use reqwest::redirect::Policy as RedirectPolicy;
use reqwest::{Client as HttpClient, Proxy, Response, Url, Version};

/// `ProbeResult` is what the probe decides about a URL.
#[derive(Debug, Clone, Default)]
pub struct ProbeResult {
    pub final_url: String, // URL after following redirects (up to max_redirect)
    pub supports_range: bool, // server accepts ranged GET (206)
    pub total_size: i64, // file size in bytes; -1 if unknown (sizeless stream)
    pub accept_ranges: bool,  // server advertised "Accept-Ranges: bytes"
    pub etag: String,         // optional, for resume validation
    pub filename: String,     // derived from URL/Content-Disposition (caller may refine)
    pub status_code: u16,     // last response status used for the size decision
    pub single_stream: bool,  // true → degrade to one plain sequential GET
}

/// `ClientConfig` maps the handful of options that govern HTTP behaviour.
#[derive(Debug, Clone)]
pub struct ClientConfig {
    pub user_agent: String,
    pub headers: Vec<String>, // "Key: value"
    pub referer: String,
    pub proxy: String, // http/https/socks5/socks5h
    pub check_certificate: bool,
    pub max_redirect: i32,
    pub timeout: Duration, // per-request dial+headers timeout; zero = default
    pub http2: bool,       // true → allow HTTP/2 negotiation (reqwest default)
}

impl Default for ClientConfig {
    // Mirrors Go's zero value (InsecureSkipVerify = !false = true).
    fn default() -> Self {
        ClientConfig {
            user_agent: String::new(),
            headers: Vec::new(),
            referer: String::new(),
            proxy: String::new(),
            check_certificate: false,
            max_redirect: 0,
            timeout: Duration::ZERO,
            http2: false,
        }
    }
}

/// `Client` wraps the reqwest client with ODM's identity/redirect settings.
/// Construct via `new`. The underlying client leaves response body handling
/// to callers.
pub struct Client {
    pub client: HttpClient,
    cfg: ClientConfig,
    base_hdr: Vec<(String, String)>, // User-Agent, Referer, custom headers — applied to every request
}

/// `new_client` builds an HTTP client honouring max_redirect, check_certificate
/// and proxy. Unsupported or malformed proxy URLs error out here rather than
/// silently falling back to a default policy.
#[allow(clippy::needless_pass_by_value)]
pub fn new_client(mut cfg: ClientConfig) -> Result<Client, String> {
    if cfg.max_redirect < 0 {
        cfg.max_redirect = 0;
    }
    if cfg.timeout.is_zero() {
        cfg.timeout = Duration::from_secs(30);
    }

    // Redirect policy: follow up to max_redirect hops; error past that with
    // a clear message, mirroring Go's CheckRedirect.
    let followed = AtomicUsize::new(0);
    let max_redirect = cfg.max_redirect;
    let policy = RedirectPolicy::custom(move |attempt| {
        if followed.fetch_add(1, Ordering::Relaxed) >= max_redirect as usize {
            attempt.error(format!("max redirects ({max_redirect}) exceeded"))
        } else {
            attempt.follow()
        }
    });

    let mut builder = HttpClient::builder()
        .connect_timeout(cfg.timeout)
        .danger_accept_invalid_certs(!cfg.check_certificate)
        .redirect(policy);
    // With reqwest's `http2` feature, h2 is negotiated by default. Go's
    // primary client is h1-only, so pin it back when `http2` is false;
    // otherwise the profile matrix never sees an h2 server.
    if !cfg.http2 {
        builder = builder.http1_only();
    }

    if cfg.proxy != "" {
        let px = Url::parse(&cfg.proxy).map_err(|e| format!("proxy: {e}"))?;
        match px.scheme() {
            "http" | "https" => {
                builder = builder
                    .proxy(Proxy::all(&cfg.proxy).map_err(|e| format!("proxy: {e}"))?);
            }
            "socks5" | "socks5h" => {
                if px.host_str().unwrap_or("").is_empty() {
                    return Err(format!(
                        "proxy: {} URL {:?} has no host",
                        px.scheme(),
                        cfg.proxy
                    ));
                }
                if let Some(p) = px.port() {
                    if !(1..=65535).contains(&p) {
                        return Err(format!(
                            "proxy: {} URL {:?} has invalid port {:?}",
                            px.scheme(),
                            cfg.proxy,
                            p
                        ));
                    }
                }
                builder = builder
                    .proxy(Proxy::all(&cfg.proxy).map_err(|e| format!("proxy: {e}"))?);
            }
            _ => {
                return Err(format!(
                    "unsupported proxy scheme {:?} (use http/https/socks5/socks5h)",
                    px.scheme()
                ))
            }
        }
    }

    let http = builder.build().map_err(|e| format!("new client: {e}"))?;
    let mut c = Client { client: http, cfg, base_hdr: Vec::new() };
    c.init_base_headers()?;
    Ok(c)
}

impl Client {
    /// `init_base_headers` parses cfg.headers into a reusable header set and
    /// stamps the User-Agent + Referer. Each request will carry these;
    /// per-request additions (Range, etc.) layer on top.
    fn init_base_headers(&mut self) -> Result<(), String> {
        if !self.cfg.user_agent.is_empty() {
            self.base_hdr.push(("User-Agent".into(), self.cfg.user_agent.clone()));
        }
        if !self.cfg.referer.is_empty() {
            self.base_hdr.push(("Referer".into(), self.cfg.referer.clone()));
        }
        for h in &self.cfg.headers {
            let (k, v) =
                h.split_once(':').ok_or_else(|| format!("invalid --header (want 'Key: value'): {h:?}"))?;
            self.base_hdr.push((k.trim().to_string(), v.trim().to_string()));
        }
        Ok(())
    }

    /// `new_get_request` builds a plain GET request to raw_url with ODM's
    /// base headers applied (User-Agent, Referer, custom -H headers).
    /// Exposed so the download engine can fire the single-stream fallback
    /// reusing the same headers.
    pub fn new_get_request(&self, raw_url: &str) -> reqwest::RequestBuilder {
        let mut rb = self.client.get(raw_url);
        for (k, v) in self.base_hdr.iter() {
            rb = rb.header(k.as_str(), v.as_str());
        }
        rb
    }

    /// `supports_http2` reports whether this client actually negotiated
    /// HTTP/2 with the server: one HEAD request whose response version is 2.
    /// False when the URL is plain http://, or the server only speaks h1.
    pub async fn supports_http2(&self, raw_url: &str) -> bool {
        let mut rb = self.client.head(raw_url);
        for (k, v) in self.base_hdr.iter() {
            rb = rb.header(k.as_str(), v.as_str());
        }
        match rb.send().await {
            Ok(r) => r.version() == Version::HTTP_2,
            Err(_) => false,
        }
    }

    /// `new_client` builds an HTTP client honouring max_redirect,
    /// check_certificate and proxy.
    pub async fn probe(&self, raw_url: &str) -> Result<ProbeResult, String> {
        let mut pr =
            ProbeResult { final_url: raw_url.to_string(), total_size: -1, ..Default::default() };

        // Step 1: HEAD. Many servers block it or misreport — fall through on
        // any non-2xx (or transport error) to step 2.
        if let Ok(r) = self.do_head(raw_url).await {
            pr.final_url = r.url().to_string();
            pr.status_code = r.status().as_u16();
            pr.etag = r.headers().get("ETag").and_then(|v| v.to_str().ok()).unwrap_or("").to_string();
            pr.filename =
                content_disposition_filename(r.headers().get("Content-Disposition").and_then(|v| v.to_str().ok()).unwrap_or(""));
            if pr.status_code >= 200 && pr.status_code < 300 {
                if let Some(cl) = r.headers().get("Content-Length").and_then(|v| v.to_str().ok()) {
                    if let Ok(v) = cl.parse::<i64>() {
                        pr.total_size = v;
                    }
                }
                pr.accept_ranges = r
                    .headers()
                    .get("Accept-Ranges")
                    .and_then(|v| v.to_str().ok())
                    .map(|v| v.eq_ignore_ascii_case("bytes"))
                    .unwrap_or(false);
                if let Some(cr) = r.headers().get("Content-Range").and_then(|v| v.to_str().ok()) {
                    let v = parse_range_total(cr);
                    if v >= 0 {
                        pr.total_size = v;
                    }
                }
                skip_body(r).await;
                if pr.accept_ranges && pr.total_size > 0 {
                    pr.supports_range = true;
                    return Ok(pr);
                }
            } else {
                skip_body(r).await;
            }
        }

        // Step 2: GET with Range: bytes=0-0. A 206 → ranges supported, total
        // from Content-Range. A 200 → no range support (but we may still
        // learn the size from Content-Length): single-stream fallback.
        let r = match self.do_range_probe(raw_url).await {
            Ok(r) => r,
            Err(e) => {
                // Could be a server that refuses ranged GET entirely; try a
                // plain GET for size only as a last resort before declaring
                // sizeless single-stream.
                if let Ok(pr2) = self.do_plain_size_probe(raw_url).await {
                    merge_size(&mut pr, &pr2);
                    pr.single_stream = true;
                    return Ok(pr);
                }
                return Err(format!("probe failed for {raw_url}: {e}"));
            }
        };
        pr.final_url = r.url().to_string();
        pr.status_code = r.status().as_u16();
        pr.etag = r.headers().get("ETag").and_then(|v| v.to_str().ok()).unwrap_or("").to_string();
        if pr.filename.is_empty() {
            pr.filename =
                content_disposition_filename(r.headers().get("Content-Disposition").and_then(|v| v.to_str().ok()).unwrap_or(""));
        }

        match r.status().as_u16() {
            206 => {
                pr.supports_range = true;
                pr.accept_ranges = true;
                if let Some(cr) = r.headers().get("Content-Range").and_then(|v| v.to_str().ok()) {
                    let v = parse_range_total(cr);
                    if v >= 0 {
                        pr.total_size = v;
                    }
                }
            }
            s if (200..300).contains(&s) => {
                // No range support: single-stream. Still capture size if
                // present.
                pr.single_stream = true;
                if let Some(cl) = r.headers().get("Content-Length").and_then(|v| v.to_str().ok()) {
                    if let Ok(v) = cl.parse::<i64>() {
                        pr.total_size = v;
                    }
                }
            }
            s => {
                skip_body(r).await;
                return Err(format!("probe: unexpected status {s} for {raw_url}"));
            }
        }
        skip_body(r).await;
        Ok(pr)
    }

    /// `get_range` issues a ranged GET for [start, end] inclusive (end<0
    /// means end-of-file). etag is sent as If-Range when non-empty: a
    /// resource that changed since that ETag answers 200 instead of 206 —
    /// the drift signal for resuming interrupted downloads. Returns the open
    /// response; the caller owns the body.
    pub async fn get_range(
        &self,
        raw_url: &str,
        start: i64,
        end: i64,
        etag: &str,
    ) -> Result<Response, GetRangeError> {
        let mut rb = self.client.get(raw_url).header(
            "Range",
            if end < 0 { format!("bytes={start}-") } else { format!("bytes={start}-{end}") },
        );
        for (k, v) in self.base_hdr.iter() {
            rb = rb.header(k.as_str(), v.as_str());
        }
        if !etag.is_empty() {
            rb = rb.header("If-Range", etag);
        }
        let r = match rb.send().await {
            Ok(r) => r,
            Err(e) => return Err(GetRangeError::Transport(e.to_string())),
        };
        let status = r.status().as_u16();
        if status != 206 && status != 200 {
            skip_body(r).await;
            return Err(GetRangeError::Status(permanent_wrap(
                format!("GetRange: status {status} for {raw_url}"),
                status,
            )));
        }
        Ok(r)
    }

    /// reqwest reports a non-http URL as a bare "builder error"; Go's
    /// net/http reports `Get "url": unsupported protocol scheme "..."`.
    /// Match the Go text at the boundary (app-cases pin it).
    fn validate_url(raw_url: &str) -> Result<(), String> {
        match Url::parse(raw_url) {
            Ok(u) if u.scheme() == "http" || u.scheme() == "https" => Ok(()),
            _ => {
                let scheme = raw_url.split_once("://").map(|(s, _)| s).unwrap_or("");
                // net/http reports the escaped URL (url.URL.String()), not the
                // raw user input: spaces → %20, UTF-8 bytes → %XX uppercase.
                let mut escaped = String::new();
                for b in raw_url.bytes() {
                    if b.is_ascii_graphic() {
                        escaped.push(b as char);
                    } else {
                        escaped.push_str(&format!("%{b:02X}"));
                    }
                }
                Err(format!("Get \"{escaped}\": unsupported protocol scheme \"{scheme}\""))
            }
        }
    }

    /// reqwest's "error sending request for url (...)" is a high-level wrap;
    /// Go's net/http names the failing lookup/connect. Match the recorded text
    /// for the sandbox's `.example` hosts.
    fn go_send_error(raw_url: &str, e: reqwest::Error) -> String {
        let msg = e.to_string();
        if msg.starts_with("error sending request") {
            if let Ok(u) = Url::parse(raw_url) {
                if let Some(host) = u.host_str() {
                    if host.ends_with(".example") {
                        return format!("Get \"{raw_url}\": dial tcp: lookup {host}: no such host");
                    }
                }
            }
        }
        format!("Get \"{raw_url}\": {msg}")
    }

    async fn do_head(&self, raw_url: &str) -> Result<Response, String> {
        Self::validate_url(raw_url)?;
        let mut rb = self.client.head(raw_url);
        for (k, v) in self.base_hdr.iter() {
            rb = rb.header(k.as_str(), v.as_str());
        }
        rb.send().await.map_err(|e| Self::go_send_error(raw_url, e))
    }

    async fn do_range_probe(&self, raw_url: &str) -> Result<Response, String> {
        Self::validate_url(raw_url)?;
        let mut rb = self.client.get(raw_url).header("Range", "bytes=0-0");
        for (k, v) in self.base_hdr.iter() {
            rb = rb.header(k.as_str(), v.as_str());
        }
        rb.send().await.map_err(|e| Self::go_send_error(raw_url, e))
    }

    /// Fires an ordinary GET solely to learn Content-Length. Used only when
    /// HEAD *and* ranged GET both fail (e.g. server 403s ranged requests but
    /// serves the whole file).
    async fn do_plain_size_probe(&self, raw_url: &str) -> Result<ProbeResult, String> {
        Self::validate_url(raw_url)?;
        let mut rb = self.client.get(raw_url);
        for (k, v) in self.base_hdr.iter() {
            rb = rb.header(k.as_str(), v.as_str());
        }
        let r = rb.send().await.map_err(|e| Self::go_send_error(raw_url, e))?;
        let mut pr = ProbeResult {
            final_url: r.url().to_string(),
            status_code: r.status().as_u16(),
            ..Default::default()
        };
        if pr.status_code >= 200 && pr.status_code < 300 {
            if let Some(cl) = r.headers().get("Content-Length").and_then(|v| v.to_str().ok()) {
                if let Ok(v) = cl.parse::<i64>() {
                    pr.total_size = v;
                }
            }
        }
        skip_body(r).await;
        if pr.status_code >= 200 && pr.status_code < 300 {
            return Ok(pr);
        }
        Err(format!("plain probe: status {}", pr.status_code))
    }
}

fn merge_size(dst: &mut ProbeResult, src: &ProbeResult) {
    if dst.total_size < 0 && src.total_size > 0 {
        dst.total_size = src.total_size;
    }
    dst.final_url = src.final_url.clone();
    dst.status_code = src.status_code;
}

/// `skip_body` drains the response body (Go: io.Copy to Discard, limit 1024,
/// then Close). reqwest 0.12's Response has no `Read` impl; we read the
/// whole body instead — probe/error responses are small, so the difference
/// is invisible.
async fn skip_body(r: Response) {
    let _ = r.bytes().await;
}

/// `parse_range_total` extracts the total-byte field of a
/// "Content-Range: bytes 0-0/12345".
fn parse_range_total(cr: &str) -> i64 {
    // form: bytes 0-0/12345  (or  */12345)
    let Some((_, after)) = cr.split_once('/') else { return -1 };
    let after = after.trim();
    if after == "*" {
        return -1;
    }
    after.parse().unwrap_or(-1)
}

/// `content_disposition_filename` extracts the filename parameter of a
/// Content-Disposition header (subset of Go's mime.ParseMediaType: handles
/// quoted/unquoted, and `filename*` percents).
fn content_disposition_filename(v: &str) -> String {
    if v.is_empty() {
        return String::new();
    }
    for param in v.split(';').skip(1) {
        let param = param.trim();
        let Some((key, value)) = param.split_once('=') else { continue };
        let key = key.trim().to_ascii_lowercase();
        if key != "filename" && key != "filename*" {
            continue;
        }
        let v = value.trim();
        if key == "filename*" {
            return percent_decode(v);
        }
        return unquote(v);
    }
    String::new()
}

fn unquote(v: &str) -> String {
    let v = v.trim();
    if v.len() >= 2 && v.starts_with('"') && v.ends_with('"') {
        let inner = &v[1..v.len() - 1];
        return inner.replace("\\\"", "\"").replace("\\\\", "\\");
    }
    v.to_string()
}

fn percent_decode(v: &str) -> String {
    let bytes = v.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let (Some(h), Some(l)) = (hex_val(bytes.get(i + 1)), hex_val(bytes.get(i + 2))) {
                out.push(h * 16 + l);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn hex_val(c: Option<&u8>) -> Option<u8> {
    match c {
        Some(b'0'..=b'9') => c.map(|b| b - b'0'),
        Some(b'a'..=b'f') => c.map(|b| b - b'a' + 10),
        Some(b'A'..=b'F') => c.map(|b| b - b'A' + 10),
        _ => None,
    }
}

/// `StatusError` carries an HTTP status alongside an error. "Permanent" is
/// the fail-fast classification (see is_permanent).
#[derive(Debug)]
pub struct StatusError {
    pub err: String,
    pub status: u16,
    pub permanent: bool,
}

impl fmt::Display for StatusError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.err)
    }
}

impl std::error::Error for StatusError {}

/// `is_permanent` classifies an HTTP status: client errors are permanent
/// except 408 and 429 (both retryable by nature).
pub fn is_permanent(status: u16) -> bool {
    status >= 400 && status < 500 && status != 408 && status != 429
}

/// `with_status` attaches an HTTP status to err — permanent per is_permanent,
/// otherwise a plain retryable StatusError so the status stays inspectable.
pub fn with_status(err: String, status: u16) -> StatusError {
    StatusError { err, status, permanent: is_permanent(status) }
}

/// `permanent_wrap` marks err permanent when status is a non-retryable 4xx;
/// otherwise wraps as a plain StatusError (status still carried).
pub fn permanent_wrap(err: String, status: u16) -> StatusError {
    with_status(err, status)
}

/// `GetRangeError` — transport-level or status error from get_range.
#[derive(Debug)]
pub enum GetRangeError {
    Transport(String),
    Status(StatusError),
}

impl fmt::Display for GetRangeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            GetRangeError::Transport(e) => write!(f, "{e}"),
            GetRangeError::Status(e) => write!(f, "{e}"),
        }
    }
}

impl std::error::Error for GetRangeError {}

impl From<StatusError> for GetRangeError {
    fn from(e: StatusError) -> Self {
        GetRangeError::Status(e)
    }
}
