//! Streaming SHA-256 backed by the C shim in `c/sha256_evp.c`
//! (OpenSSL libcrypto EVP, same backend as the `openssl` crate binding).

use std::ffi::c_void;
use std::ptr;

unsafe extern "C" {
    fn odm_sha256_new() -> *mut c_void;
    fn odm_sha256_update(ctx: *mut c_void, data: *const u8, len: usize);
    fn odm_sha256_final(ctx: *mut c_void, out: *mut u8);
    fn odm_sha256_free(ctx: *mut c_void);
}

pub struct FastSha256(*mut c_void);

// SAFETY: each context is owned by one worker and never shared across threads.
unsafe impl Send for FastSha256 {}

impl FastSha256 {
    pub fn new() -> Self {
        let ctx = unsafe { odm_sha256_new() };
        assert!(!ctx.is_null(), "EVP SHA-256 context allocation failed");
        FastSha256(ctx)
    }

    pub fn update(&mut self, b: &[u8]) {
        unsafe { odm_sha256_update(self.0, b.as_ptr(), b.len()) }
    }

    pub fn finalize(mut self) -> [u8; 32] {
        let mut buf = [0u8; 32];
        unsafe { odm_sha256_final(self.0, buf.as_mut_ptr()) };
        self.free_ctx();
        buf
    }

    fn free_ctx(&mut self) {
        if !self.0.is_null() {
            unsafe { odm_sha256_free(self.0) };
            self.0 = ptr::null_mut();
        }
    }
}

impl Default for FastSha256 {
    fn default() -> Self {
        Self::new()
    }
}

impl Drop for FastSha256 {
    fn drop(&mut self) {
        self.free_ctx();
    }
}

#[cfg(test)]
mod tests {
    use super::FastSha256;

    fn digest(bytes: &[u8]) -> String {
        let mut h = FastSha256::new();
        for chunk in bytes.chunks(7919) {
            h.update(chunk);
        }
        h.finalize().iter().map(|b| format!("{b:02x}")).collect()
    }

    #[test]
    fn matches_sha256_vectors() {
        assert_eq!(
            digest(b""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert_eq!(
            digest(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(
            digest(&vec![b'a'; 1024 * 1024]),
            "9bc1b2a288b26af7257a36277ae3816a7d4f16e89c1e7e77d0a5c48bad62b360"
        );
    }
}
