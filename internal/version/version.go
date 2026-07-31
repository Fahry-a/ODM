// Package version is the single source of truth for ODM's release string.
//
// Historically the version was duplicated in internal/config and
// internal/download (an import cycle prevented sharing), and the two could
// drift apart. config.Version and download.Version are now compile-time
// aliases of this const, so a mismatch is a build error, not a release risk.
//
// The value is mirrored in packaging/PKGBUILD (pkgver) — CI enforces that the
// Go value and pkgver stay in sync (see AGENTS.md "Releases").
package version

// Version is the ODM release string; baked into --version, the default
// User-Agent, and the RPC odm.getVersion response.
const Version = "odm/1.2.0"
