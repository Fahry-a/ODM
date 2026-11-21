use std::collections::{BTreeMap, HashSet};
use std::fmt;
use std::fs::{self, OpenOptions};
use std::io;

use serde::{Deserialize, Serialize};

#[cfg(unix)]
use std::os::unix::fs::OpenOptionsExt;

/// `ControlFile` is the JSON payload of `<filename>.odm`. It records enough
/// state to resume: the source URL, the total size, chunk size, and the set
/// of chunk indices already written. On a resume we re-queue only the
/// missing chunks.
///
/// Extended fields (v0.2.0+) provide richer diagnostics and validation.
/// ChunkHashes records the lowercase hex SHA-256 of every completed chunk's
/// bytes, keyed by the chunk's Start byte offset — a resume verifies these
/// against local disk. Legacy files without the map fall back to the
/// server-side compare.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ControlFile {
    // Core fields (v0.1.0)
    #[serde(default)]
    pub url: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub final_url: Option<String>, // post-redirect URL actually used
    #[serde(default)]
    pub total_size: i64,
    #[serde(default)]
    pub chunk_size: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub etag: Option<String>,
    // Sorted chunk-byte-offsets already written (mirrors Go's nil slice → null).
    #[serde(default)]
    pub completed: Option<Vec<i64>>,

    // Extended fields (v0.2.0+) — omitted when unset.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_at: Option<String>, // when this control file was first written
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<String>, // last checkpoint timestamp
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub connections: Option<i64>, // parallel connections used
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user_agent: Option<String>, // UA sent to server
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub odm_version: Option<String>, // version that created this file
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub checksum: Option<String>, // "algo:hex" if --checksum was used

    /// Per-chunk SHA-256 digests for resume verification:
    /// key = chunk Start byte offset, value = lowercase hex sha256 of that
    /// chunk's bytes. JSON object keys marshal as strings, so integer keys
    /// round-trip cleanly. Missing map (None) = legacy control file; resume
    /// falls back to the server-side sample compare.
    #[serde(default, skip_serializing_if = "none_or_empty")]
    pub chunk_hashes: Option<BTreeMap<i64, String>>,

    /// Engine profile metadata (v0.3.0+) — "odm"|"aria2c"|"both"; empty = legacy.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub profile: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub split_at: Option<i64>, // region boundary (both); 0 = no split
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub region2_chunk_size: Option<i64>, // region2 segment size (both)
}

fn none_or_empty(m: &Option<BTreeMap<i64, String>>) -> bool {
    m.as_ref().map_or(true, |m| m.is_empty())
}

/// Returned by `load_control` when the `.odm` file is absent.
#[derive(Debug)]
pub enum LoadError {
    NotExist,
    Io(io::Error),
    Parse(serde_json::Error),
}

impl fmt::Display for LoadError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            LoadError::NotExist => write!(f, "no control file"),
            LoadError::Io(e) => write!(f, "read control: {e}"),
            LoadError::Parse(e) => write!(f, "parse control: {e}"),
        }
    }
}

impl std::error::Error for LoadError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            LoadError::NotExist => None,
            LoadError::Io(e) => Some(e),
            LoadError::Parse(e) => Some(e),
        }
    }
}

impl From<io::Error> for LoadError {
    fn from(e: io::Error) -> Self {
        LoadError::Io(e)
    }
}

/// `control_path` returns the `.odm` path that accompanies a destination file.
pub fn control_path(dest_path: &str) -> String {
    format!("{dest_path}.odm")
}

/// `load_control` reads and decodes the control file for dest_path. Missing
/// file → `LoadError::NotExist` (distinct from a corrupt file, which is a real
/// error).
pub fn load_control(dest_path: &str) -> Result<ControlFile, LoadError> {
    let b = fs::read(control_path(dest_path)).map_err(|e| {
        if e.kind() == io::ErrorKind::NotFound {
            LoadError::NotExist
        } else {
            LoadError::Io(e)
        }
    })?;
    serde_json::from_slice(&b).map_err(LoadError::Parse)
}

/// `save_control` writes the control file atomically (temp + rename) so a
/// crash mid-write can't leave a half-encoded file.
pub fn save_control(dest_path: &str, cf: &ControlFile) -> io::Result<()> {
    let b = serde_json::to_vec_pretty(cf).map_err(io::Error::other)?;
    let tmp = format!("{}.tmp", control_path(dest_path));
    let mut opts = OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    opts.mode(0o600);
    let mut f = opts
        .open(&tmp)
        .map_err(|e| io::Error::other(format!("write control tmp: {e}")))?;
    use std::io::Write;
    f.write_all(&b)?;
    f.sync_all()?;
    drop(f);
    fs::rename(&tmp, control_path(dest_path))
        .map_err(|e| io::Error::other(format!("rename control: {e}")))?;
    Ok(())
}

/// `remove_control` deletes the control file (called once a task verifies
/// successfully). Missing-file is not an error.
pub fn remove_control(dest_path: &str) -> io::Result<()> {
    match fs::remove_file(control_path(dest_path)) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
        Ok(()) => {}
    }
    // Also remove a stray .tmp from an interrupted save attempt.
    let _ = fs::remove_file(format!("{}.tmp", control_path(dest_path)));
    Ok(())
}

impl ControlFile {
    /// `completed_offsets` builds the set of completed chunk byte-offsets for
    /// fast membership tests at resume time. Chunks are keyed by their
    /// starting byte offset.
    pub fn completed_offsets(&self) -> HashSet<i64> {
        self.completed
            .as_ref()
            .map(|v| v.iter().copied().collect())
            .unwrap_or_default()
    }

    /// `chunk_hash` returns the recorded hash for the chunk starting at
    /// `start`, if present. Reading a None map is safe — legacy files simply
    /// report not-found.
    pub fn chunk_hash(&self, start: i64) -> Option<String> {
        self.chunk_hashes.as_ref()?.get(&start).cloned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;

    fn dest() -> String {
        static N: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let n = N.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let dir = std::env::temp_dir().join(format!(
            "odm-storage-resume-test-{}-{n}",
            std::process::id()
        ));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        dir.join("file.tar.gz").to_str().unwrap().to_string()
    }

    #[test]
    fn test_control_path() {
        let cases = [
            ("/tmp/blob.bin", "/tmp/blob.bin.odm"),
            ("rel.tar", "rel.tar.odm"),
            ("/x/y.z/file", "/x/y.z/file.odm"),
            ("", ".odm"),
        ];
        for (input, want) in cases {
            assert_eq!(control_path(input), want);
        }
    }

    #[test]
    fn test_completed_offsets_builds_membership_set() {
        let cf = ControlFile {
            completed: Some(vec![0, 4096, 8192]),
            ..Default::default()
        };
        let m = cf.completed_offsets();
        assert_eq!(m.len(), 3);
        for k in [0, 4096, 8192] {
            assert!(m.contains(&k), "missing offset {k}");
        }
        assert!(!m.contains(&1234));
    }

    #[test]
    fn test_save_load_control_round_trip() {
        let d = dest();
        let expected = ControlFile {
            url: "https://example.com/file.tar.gz".into(),
            final_url: Some("https://cdn.example.com/file.tar.gz".into()),
            total_size: 1 << 20,
            chunk_size: 4 << 10,
            etag: Some("\"deadbeef\"".into()),
            completed: Some(vec![0, 4096, 8192, 12288]),
            ..Default::default()
        };
        save_control(&d, &expected).unwrap();
        assert!(Path::new(&control_path(&d)).exists(), "control file not written");
        let out = load_control(&d).unwrap();
        assert_eq!(out, expected, "round-trip mismatch\nexpected={expected:?}\nactual={out:?}");
    }

    #[test]
    fn test_load_control_missing_file_is_not_exist() {
        let dir = std::env::temp_dir().join(format!(
            "odm-storage-resume-missing-{}",
            std::process::id()
        ));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let d = dir.join("missing.bin").to_str().unwrap().to_string();
        match load_control(&d) {
            Err(LoadError::NotExist) => {}
            other => panic!("want NotExist, got {other:?}"),
        }
    }

    #[test]
    fn test_load_control_corrupt_file_is_error() {
        let d = dest();
        fs::write(control_path(&d), "{not json").unwrap();
        assert!(load_control(&d).is_err(), "want error for corrupt control file");
    }

    #[test]
    fn test_save_control_atomic_writes_json() {
        let d = dest();
        let cf = ControlFile {
            url: "u".into(),
            total_size: 10,
            chunk_size: 2,
            completed: Some(vec![0]),
            ..Default::default()
        };
        save_control(&d, &cf).unwrap();
        let b = fs::read(control_path(&d)).unwrap();
        let raw: serde_json::Value = serde_json::from_slice(&b).unwrap();
        assert_eq!(raw["url"], "u");
        assert_eq!(raw["total_size"].as_i64(), Some(10));
        assert!(raw.get("etag").is_none(), "etag should be omitted when unset");
    }

    #[test]
    fn test_save_control_no_leftover_tmp() {
        let d = dest();
        save_control(&d, &ControlFile { url: "u".into(), ..Default::default() }).unwrap();
        assert!(!Path::new(&format!("{}.tmp", control_path(&d))).exists());
    }

    #[test]
    fn test_save_load_chunk_hashes_round_trip() {
        let d = dest();
        let expected = ControlFile {
            url: "https://example.com/hashed.bin".into(),
            total_size: 1024,
            chunk_size: 256,
            completed: Some(vec![0, 256]),
            chunk_hashes: Some(BTreeMap::from([
                (0i64, "a".repeat(64)),
                (256i64, "b".repeat(64)),
            ])),
            ..Default::default()
        };
        save_control(&d, &expected).unwrap();
        let out = load_control(&d).unwrap();
        assert_eq!(out.chunk_hashes, expected.chunk_hashes);
        assert_eq!(out.completed, expected.completed);
    }

    #[test]
    fn test_load_control_legacy_without_chunk_hashes() {
        let d = dest();
        let legacy = r#"{
  "url": "https://example.com/legacy.bin",
  "total_size": 1024,
  "chunk_size": 256,
  "completed": [0, 256]
}"#;
        fs::write(control_path(&d), legacy).unwrap();
        let cf = load_control(&d).unwrap();
        assert!(cf.chunk_hashes.is_none());
        assert_eq!(cf.completed, Some(vec![0, 256]));
    }

    #[test]
    fn test_chunk_hash_helpers() {
        let mut cf = ControlFile::default();
        assert!(cf.chunk_hash(0).is_none(), "legacy nil map must report not-found");
        cf.chunk_hashes = Some(BTreeMap::from([(4096i64, "deadbeef".to_string())]));
        assert_eq!(cf.chunk_hash(4096).as_deref(), Some("deadbeef"));
        assert!(cf.chunk_hash(8192).is_none());
    }

    #[test]
    fn test_save_control_omits_empty_chunk_hashes() {
        let d = dest();
        save_control(&d, &ControlFile { url: "u".into(), ..Default::default() }).unwrap();
        let b = fs::read(control_path(&d)).unwrap();
        let raw: serde_json::Value = serde_json::from_slice(&b).unwrap();
        assert!(raw.get("chunk_hashes").is_none());
    }

    #[test]
    fn test_remove_control_idempotent() {
        let d = dest();
        remove_control(&d).unwrap();
        save_control(&d, &ControlFile { url: "u".into(), ..Default::default() }).unwrap();
        remove_control(&d).unwrap();
        remove_control(&d).unwrap();
    }

    #[test]
    fn test_remove_control_cleans_stray_tmp() {
        let d = dest();
        fs::write(format!("{}.tmp", control_path(&d)), "partial").unwrap();
        remove_control(&d).unwrap();
        assert!(!Path::new(&format!("{}.tmp", control_path(&d))).exists());
    }
}
