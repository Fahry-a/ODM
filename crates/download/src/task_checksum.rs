//! Rust port of ODM's `internal/download/task_checksum.go` — fetchChecksumSpec,
//! parseChecksumSidecar, Task::verifyChecksum.

use std::time::Duration;

use crate::task::{Ctx, Error, Task};

impl Task {
    /// `fetch_checksum_spec` — downloads the sidecar and parses "algo:hex"
    /// (or a bare hash with the digest's length, or the "sha256sum"-style
    /// trailing filename after the hash). Go bounds the fetch with a 30s ctx;
    /// a hung sidecar server must not pin the task forever.
    pub async fn fetch_checksum_spec(&self, ctx: &Ctx) -> Result<String, Error> {
        let fetch = async {
            let url = self.options().checksum_url.clone();
            let rb = self.client.new_get_request(&url);
            let resp = rb.send().await.map_err(|e| Error::msg(e.to_string()))?;
            let status = resp.status().as_u16();
            if !(200..300).contains(&status) {
                let _ = resp.bytes().await;
                return Err(Error::msg(format!("GET {url}: status {status}")));
            }
            // Go: io.ReadAll(io.LimitReader(resp.Body, 64*1024)).
            let b = resp.bytes().await.map_err(|e| Error::msg(e.to_string()))?;
            let end = b.len().min(64 * 1024);
            Ok(parse_checksum_sidecar(String::from_utf8_lossy(&b[..end]).into_owned())?)
        };
        tokio::select! {
            r = tokio::time::timeout(Duration::from_secs(30), fetch) => {
                r.map_err(|_| Error::msg("checksum sidecar fetch timed out"))?
            }
            _ = ctx.wait() => Err(Error::msg("cancelled during checksum sidecar fetch")),
        }
    }

    /// `verify_checksum` — verify against t.out_path (real written file,
    /// which may be renamed via Content-Disposition).
    pub fn verify_checksum(&self) -> Result<(), Error> {
        let spec = self.options().checksum.clone();
        if spec.is_empty() {
            return Ok(());
        }
        let Some((algo, hex_str)) = spec.split_once(':') else {
            return Err(Error::msg(format!("checksum: bad spec {spec:?}")));
        };
        if hex_str.is_empty() {
            return Err(Error::msg(format!("checksum: bad spec {spec:?}")));
        }
        crate::manager::verify_checksum(&self.output_path(), algo, hex_str)
    }
}

/// `parse_checksum_sidecar` — accepts "algo:<hash>", "<64-hex>" (sha256),
/// "<40-hex>" (sha1), "<32-hex>" (md5), optionally followed by the filename.
pub fn parse_checksum_sidecar(s: String) -> Result<String, Error> {
    let fields: Vec<String> = s.split_whitespace().map(|w| w.to_string()).collect();
    let Some(tok) = fields.first() else {
        return Err(Error::msg("empty checksum file"));
    };
    let (mut algo, hex_str) = tok.split_once(':').unwrap_or(("", tok));
    if algo.is_empty() {
        algo = match hex_str.len() {
            64 => "sha256",
            40 => "sha1",
            32 => "md5",
            _ => return Err(Error::msg(format!("unrecognised digest form {tok:?}"))),
        };
    }
    match algo.to_lowercase().as_str() {
        "md5" | "sha1" | "sha256" => {}
        _ => return Err(Error::msg(format!("unsupported algorithm {algo:?}"))),
    }
    Ok(format!("{}:{}", algo.to_lowercase(), hex_str.to_lowercase()))
}
