//! Rust port of ODM's `internal/download/smart.go`: the decision matrix for
//! the smart profile (after probing + h2 readiness check).

/// `ServerCapabilities` are the inputs the smart decision needs.
#[derive(Debug, Clone, Copy)]
pub struct ServerCapabilities {
    pub total_size: i64,
    pub supports_range: bool,
    pub single_stream: bool,
    pub http2_ready: bool,
    pub conns: i32,
}

/// `choose_profile` — the best engine profile + a short reason:
///   1. no range support / sizeless / single-stream → odm
///   2. totalSize < 8 MiB → odm (split overhead not worth it)
///   3. server doesn't speak h2 → odm
///   4. conns ≤ 2 → odm
///   5. totalSize ≥ 256 MiB && conns ≥ 6 → both
///   6. otherwise → aria2c
pub fn choose_profile(c: ServerCapabilities) -> (String, String) {
    if c.single_stream || !c.supports_range || c.total_size <= 0 {
        return ("odm".to_string(), "no range support".to_string());
    }
    if c.total_size < 8 << 20 {
        return ("odm".to_string(), "small file".to_string());
    }
    if !c.http2_ready {
        return ("odm".to_string(), "no h2".to_string());
    }
    if c.conns <= 2 {
        return ("odm".to_string(), "low conns".to_string());
    }
    if c.total_size >= 256 << 20 && c.conns >= 6 {
        return ("both".to_string(), "large+wide".to_string());
    }
    ("aria2c".to_string(), "default h2".to_string())
}
