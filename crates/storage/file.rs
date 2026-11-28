use std::fs::{File as OsFile, OpenOptions};
use std::io::self;
use std::path::{Path, PathBuf};

#[cfg(unix)]
use std::os::unix::fs::OpenOptionsExt;

/// `File` wraps a destination file for concurrent chunk writes +
/// pre-allocation.
///
/// Chunk writes never overlap (chunk boundaries are disjoint), so concurrent
/// positioned writes are safe without a lock — the OS positions the write at
/// the given offset, mirroring Go's `os.File.WriteAt`. We hold a single
/// `OsFile` per task.
#[derive(Debug)]
pub struct File {
    path: PathBuf,
    f: Option<OsFile>,
}

impl File {
    /// Opens (creating as needed) the destination file at dir/name. If size is
    /// known (>0), the file is pre-allocated to size so chunk writes can land
    /// at any offset without the file growing mid-write. size<0 keeps it as a
    /// plain grow-as-you-write file for sizeless streams.
    pub fn open_for_write(dir: &str, name: &str, size: i64) -> io::Result<File> {
        if name.is_empty() {
            return Err(io::Error::new(io::ErrorKind::InvalidInput, "no output filename"));
        }
        let dir = if dir.is_empty() { "." } else { dir };
        std::fs::create_dir_all(dir)
            .map_err(|e| io::Error::other(format!("mkdir {dir}: {e}")))?;
        let path = Path::new(dir).join(name);
        let mut opts = OpenOptions::new();
        opts.read(true).write(true).create(true);
        #[cfg(unix)]
        opts.mode(0o644);
        let f = opts
            .open(&path)
            .map_err(|e| io::Error::other(format!("open {}: {e}", path.display())))?;
        if size > 0 {
            // ftruncate-equivalent: reserve the size; sparse on filesystems
            // that support it.
            f.set_len(size as u64)
                .map_err(|e| io::Error::other(format!("truncate {} to {size}: {e}", path.display())))?;
        }
        Ok(File { path, f: Some(f) })
    }

    /// Writes b at the given offset. Safe for concurrent use ONLY when
    /// callers write non-overlapping byte ranges (the download engine
    /// guarantees this via chunk boundaries).
    pub fn write_at(&self, b: &[u8], off: i64) -> io::Result<usize> {
        match &self.f {
            Some(f) => positioned_write(f, b, off),
            None => Err(io::Error::new(io::ErrorKind::NotConnected, "file closed")),
        }
    }

    /// ReadAt is used by checksum verify after the download completes.
    pub fn read_at(&self, p: &mut [u8], off: i64) -> io::Result<usize> {
        match &self.f {
            Some(f) => positioned_read(f, p, off),
            None => Err(io::Error::new(io::ErrorKind::NotConnected, "file closed")),
        }
    }

    /// Sync flushes pending writes to disk (called once when a task
    /// completes).
    pub fn sync(&self) -> io::Result<()> {
        match &self.f {
            Some(f) => f.sync_all(),
            None => Err(io::Error::new(io::ErrorKind::NotConnected, "file closed")),
        }
    }

    /// Close closes the underlying file. Idempotent: a second call is a
    /// no-op, so the engine can call it defensively on both a defer and an
    /// explicit shutdown path.
    pub fn close(&mut self) -> io::Result<()> {
        if let Some(f) = self.f.take() {
            drop(f);
        }
        Ok(())
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Appends the whole slice at the current end of file (used for log
    /// mirrors; Go's logger appends via O_APPEND).
    pub fn append(&self, b: &[u8]) -> io::Result<()> {
        let f = match &self.f {
            Some(f) => f,
            None => return Err(io::Error::new(io::ErrorKind::NotConnected, "file closed")),
        };
        let mut off = f.metadata().map(|m| m.len() as i64).unwrap_or(0);
        let mut written = 0usize;
        while written < b.len() {
            let n = positioned_write(f, &b[written..], off)?;
            off += n as i64;
            written += n;
            if n == 0 {
                return Err(io::Error::new(io::ErrorKind::WriteZero, "append made no progress"));
            }
        }
        Ok(())
    }
}

#[cfg(unix)]
fn positioned_write(f: &OsFile, b: &[u8], off: i64) -> io::Result<usize> {
    use std::os::unix::fs::FileExt;
    f.write_at(b, off as u64)
}

#[cfg(unix)]
fn positioned_read(f: &OsFile, p: &mut [u8], off: i64) -> io::Result<usize> {
    use std::os::unix::fs::FileExt;
    f.read_at(p, off as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tempdir(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "odm-storage-test-{}-{}",
            std::process::id(),
            name
        ));
        let _ = std::fs::remove_dir_all(&dir);
        dir
    }

    #[test]
    fn test_open_for_write_preallocates() {
        let dir = tempdir("prealloc");
        let mut f = File::open_for_write(dir.to_str().unwrap(), "blob.bin", 1 << 16).unwrap();
        let _ = f.close();
        let info = std::fs::metadata(dir.join("blob.bin")).unwrap();
        assert_eq!(info.len(), (1 << 16) as u64);
    }

    #[test]
    fn test_open_for_write_sizeless_stream() {
        let dir = tempdir("stream");
        let mut f = File::open_for_write(dir.to_str().unwrap(), "stream.dat", -1).unwrap();
        let info = std::fs::metadata(dir.join("stream.dat")).unwrap();
        assert_eq!(info.len(), 0);
        f.write_at(b"hello", 0).unwrap();
        let info = std::fs::metadata(dir.join("stream.dat")).unwrap();
        assert_eq!(info.len(), 5);
        let _ = f.close();
    }

    #[test]
    fn test_open_for_write_creates_missing_dir() {
        let dir = tempdir("nested");
        let nested = dir.join("a").join("b").join("c");
        let mut f = File::open_for_write(nested.to_str().unwrap(), "x.bin", 4).unwrap();
        let _ = f.close();
        assert!(std::fs::metadata(&nested).unwrap().is_dir());
    }

    #[test]
    fn test_open_for_write_rejects_empty_name() {
        let dir = tempdir("empty-name");
        assert!(File::open_for_write(dir.to_str().unwrap(), "", 1).is_err());
    }

    #[test]
    fn test_file_write_at_read_at_round_trip() {
        let dir = tempdir("rw");
        let f = File::open_for_write(dir.to_str().unwrap(), "rw.bin", 1 << 14).unwrap();
        let payload = vec![b'Z'; 4096];
        f.write_at(&payload, 1 << 13).unwrap();
        let mut got = vec![0u8; payload.len()];
        f.read_at(&mut got, 1 << 13).unwrap();
        assert_eq!(got, payload);
        f.sync().unwrap();
    }

    #[test]
    fn test_file_concurrent_write_at_no_overlap() {
        let dir = tempdir("concurrent");
        let mut f = File::open_for_write(dir.to_str().unwrap(), "concurrent.bin", (256 * 32) as i64).unwrap();
        std::thread::scope(|scope| {
            for i in 0..32usize {
                let f = &f;
                scope.spawn(move || {
                    let payload = vec![b'A' + (i % 26) as u8; 256];
                    f.write_at(&payload, (i * 256) as i64).unwrap();
                });
            }
        });
        f.sync().unwrap();
        let mut got = vec![0u8; 256 * 32];
        f.read_at(&mut got, 0).unwrap();
        for i in 0..32usize {
            for j in 0..256 {
                assert_eq!(got[i * 256 + j], b'A' + (i % 26) as u8, "chunk {i} byte {j} torn");
            }
        }
        let _ = f.close();
    }

    #[test]
    fn test_file_close_idempotent() {
        let dir = tempdir("close");
        let mut f = File::open_for_write(dir.to_str().unwrap(), "tmp.bin", 4).unwrap();
        f.close().unwrap();
        f.close().unwrap();
    }
}
