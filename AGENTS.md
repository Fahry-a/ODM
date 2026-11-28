# AGENTS.md

## Build & test

```bash
cargo build --workspace --locked      # build all crates (workspace root Cargo.toml)
cargo test --workspace --locked       # full test suite
cargo clippy --workspace --all-targets --locked
cargo fmt --all                       # optional; format before committing
cargo build --release --locked -p odm # release binary at target/release/odm
```

CI (`.github/workflows/ci.yml`) runs `cargo build --workspace --locked`,
`cargo clippy --workspace --all-targets --locked`, the version-consistency
check, and `cargo test --workspace --locked` on every push/PR to `main`.

Static musl build (like the release artifacts). Requires Go (cgo) and musl-gcc; `.cargo/config.toml` routes linking through `scripts/musl-link.sh`, which applies the mandatory Go constructor patch — an unpatched static binary segfaults on its first Go call:

```bash
rustup target add x86_64-unknown-linux-musl
CC_x86_64_unknown_linux_musl=musl-gcc \
  cargo build --release --locked --target x86_64-unknown-linux-musl -p odm
```

The gitignored `gocore/libodmgo*.a` archives are built on demand by `crates/app/build.rs` (script: `scripts/build-go-core.sh`). For all six targets use `scripts/build-static.sh <triple>`; release cross-builds run the same patch step in `.github/scripts/cross-compile.sh`.

`build/` is gitignored — cross-compiled binaries go there.

## Workflow: TDD (mandatory)

Every fix and feature goes **red → green → refactor**: write the failing test
first, confirm it fails for the right reason, make the smallest change, then
tidy up while green. Boundary validation is tested at the boundary
(`config::setup`, RPC dispatch, `Task::start`), not against internals.

Before committing: `cargo test --workspace --locked` must pass. Concurrency
fixes should be repeat-run to exercise the race window.

## Architecture

Single binary `crates/app` (`odm`), wired from the ported internal packages:

```
crates/app    → entry point, clap flags (via config), mode dispatch
crates/config → CLI flags + config file merge, Metalink, URL resolution
crates/version→ single source of truth for VERSION (odm/X.Y.Z)
crates/download → Task lifecycle, chunk queue, Manager, exit codes
crates/transport → HTTP client, Probe, proxy (http/https/socks5/socks5h)
crates/scheduler → Connection Balancer, live Scheduler, Daemon
crates/ui     → pacman/ILoveCandy bars, prompts, TTY renderer
crates/rpc    → JSON-RPC 2.0 + WebSocket server, Broadcaster, throttler
crates/ratelimit → global token bucket (hand-rolled, no x/time/rate)
crates/storage → File WriteAt, resume control files (.odm)
crates/logging → leveled logger
crates/update → self-update + version hint
```

Two execution modes: CLI one-shot (default) and RPC daemon (`--rpc`).

## Key conventions

- Version is single-sourced in `crates/version/lib.rs` (`pub const VERSION`),
  imported by config (User-Agent), download (control-file metadata) and rpc
  (`odm.getVersion`). CI checks it stays in sync with `packaging/PKGBUILD`.
- `Snapshot()` / `Task::snapshot()` is the read API used by UI and RPC.
  Write path uses atomic fields.
- `internal/ui/render.rs` equivalent (`crates/ui/src/render.rs`) is the pure
  rendering layer; animation/TTY state lives in `progress.rs`. Bar width is
  fixed at 30 cells; ETA is `HH:MM:SS` (8 cells).
- Exit codes: 0 ok, 1 args, 2 network, 3 partial, 4 cancelled
  (`download::exit_code`).
- Chunk writes use positioned `WriteAt` on non-overlapping ranges; no global
  write lock. Per-chunk SHA-256 in `.odm` is required for resume integrity
  parity with the former Go implementation.

## Releases

Automated: push a version bump to `main` → `auto-tag.yml` validates/versions →
`release.yml` cross-compiles six targets → `aur-publish.yml` ships AUR.

Release checklist (all must match the version being released):

1. `crates/version/lib.rs` — `pub const VERSION: &str = "odm/X.Y.Z";`
2. `packaging/PKGBUILD:9` — `pkgver=X.Y.Z`
3. `CHANGELOG.md` — new `## [X.Y.Z] - YYYY-MM-DD` section and link updates

Semver from changes: new features → minor, bug fixes → patch, breaking → major.
Commit, then `git push origin main`; verify with `gh run list` and
`gh release view "vX.Y.Z"`. Manual fallback:

```bash
git tag vX.Y.Z -m "release: vX.Y.Z"
git push origin main && git push origin vX.Y.Z
```

PKGBUILD-only changes (already-released version) do NOT bump `pkgver` —
`aur-publish.yml` REPUBLISH mode handles `pkgrel`.

## Audits

A prior full-project audit (Go era) closed silent aria-segment loss, dead 429
backoff, limiter TOCTOU panic, Metalink4 multi-target corruption, RPC nil-ctx
panic and related minors; all have regression tests in the Rust port too.
When touching task/chunk/retry logic, ratelimit concurrency or Metalink
parsing, re-check those invariants first.
