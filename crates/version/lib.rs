//! Single source of truth for the ODM release string (Rust port of
//! internal/version/version.go).
//!
//! The value is mirrored in packaging/PKGBUILD (pkgver) — CI enforces that the
//! Rust value and pkgver stay in sync (see AGENTS.md "Releases").

/// ODM release string; baked into --version, the default User-Agent, and the
/// RPC odm.getVersion response.
pub const VERSION: &str = "odm/1.10.1";

#[cfg(test)]
mod tests {
    #[test]
    fn version_string_is_release_value() {
        assert_eq!(super::VERSION, "odm/1.10.1");
    }
}
