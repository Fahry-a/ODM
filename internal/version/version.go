// Package version is the single source of truth for ODM's release string.
// The value is mirrored in packaging/PKGBUILD (pkgver) — CI enforces that the
// Go value and pkgver stay in sync (see AGENTS.md "Releases").
package version

// Version is the ODM release string; baked into --version, the default
// User-Agent, and the RPC odm.getVersion response.
const Version = "odm/1.7.0"
