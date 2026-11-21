# DECISIONS — ODM (Go) → Rust

Numbered ADRs; never renumbered. Source of truth for the port. All statuses
`accepted (2026-11-20)` — the grilling round that produced them: shape CLI +
RPC daemon (ADR-001), full features (ADR-002), functional parity (ADR-003),
fully compatible flags/formats (ADR-004), deps locked to crates (ADR-005),
static binary + speed vs aria2c (ADR-006), cutover = build/odm, original
stays (ADR-007).

## ADR-001: Shape
Context: The app is a single Go binary (`cmd/odm`) with two entry forms: the
download CLI and `odm --rpc` (JSON-RPC + WebSocket daemon, default port 6900).
Decision: Keep both entry points. The Rust app has a `main` that dispatches:
no/unknown arguments = download CLI; `--rpc` = daemon.
Consequences: RPC protocol, daemon methods and flags stay identical (ADR-004);
the daemon uses tokio + tokio-tungstenite (ADR-005).

## ADR-002: Scope
Context: `analyze --whole-app` lists 12 units: storage, update, transport,
logging, version, ratelimit, config, download, scheduler, rpc, ui, cmd/odm —
incl. metalink/metalink4 input, mirrors with chunk rotation, socks5 proxy,
session-log JSONL, connection budget modes, self-update, RPC daemon.
Decision: All features in scope (user-confirmed). Magnet/BitTorrent stays a
non-goal (AGENTS.md roadmap, out of scope today).
Consequences: Port every listed unit; no feature deletions.

## ADR-003: Equivalence bar
Context: User: "gak perlu sama persi kayak byte identik. yang penting semua
fungsi, fitur dan yang lain lain nya berfungsi normal."
Decision: Functional parity: same exit codes (0/1/2/3/4), same observable
behavior and output, same on-disk and wire formats (ADR-004). Byte-identical
output is NOT required; Go implementation details (pointer addresses,
hash-order line noise, formatting of paths) may differ. The Go unit tests are
the spec; ports may idiomatic-ize after replay passes.
Consequences: Behavior per unit pinned by `app replay` against recorded cases;
known intentional behavior changes need user approval (whole-app.md step 7).

## ADR-004: Compatibility (flags/formats)
Context: CLI flags from `odm --help` are the documented contract; `.odm`
control files + per-chunk hashes drive resume; `/etc/odm/config.conf` is the
default config path; RPC is JSON-RPC 2.0 over WebSocket (`--rpc-secret`
`token:<secret>` auth, `?secret=` query auth, `--rpc-listen-port`/`-all`).
Decision: Fully compatible — all flags keep working identically, config keys
unchanged, `.odm` control-file layout + per-chunk hashes unchanged (so resumes
from the Go version still work), RPC method set unchanged. Any deviation from
this is a bug in the port.
Consequences: Version const stays `odm/1.9.2`-style? — NO: functional parity
only; if a flag/format truly cannot be preserved, note it in MAP.md as a
behavior difference and ask the user. Default User-Agent `odm/1.9.2` stays.

## ADR-005: Dependencies
Context: External deps: spf13/pflag, gorilla/websocket, golang.org/x/net/proxy,
golang.org/x/time/rate. User confirmed: lock to crates; std-only alternative
offered and declined.
Decision:
- `spf13/pflag` → `clap` (parser for the fixed flag set; default features off
  where possible). Research: docs.rs of clap, alternatives (argh/lexopt —
  see rust/research/dep-clap.md).
- `gorilla/websocket` → `tokio-tungstenite` (default rustls; `ws` feature).
- `golang.org/x/net/proxy` → `tokio-socks` (SOCKS5/5h) + hand-rolled HTTP/HTTPS
  proxy connect path (no extra crate; see research for reqwest alternative).
- `golang.org/x/time/rate` → hand-rolled global token bucket (O(1) mutex
  op). Research: see rust/research/dep-ratelimit.md.
Consequences: `cargo` build; vendored crates if offline (check network at
`bridge`/`build` time); every external dep documented in rust/research/.

## ADR-006: Targets & speed vs aria2c
Context: User: static binary (`ldd` → not a dynamic executable); "rust odm
jauh lebih cepat di semua bidang atau semuanya di bandingin aria2c."
Decision:
1. Static binary: `cargo build --release` target = musl if installed
   (`rustup target list --installed`), else `-C link-arg=-static` with
   `musl`-less glibc-static. Verify: `file build/odm` shows statically linked;
   `ldd build/odm` → "not a dynamic executable".
2. Speed goal: native async engine; buffer pool to avoid per-chunk
   allocation; single-port listen for RPC; profile-vs-aria2c benchmark at the
   end (`odm --profile aria2c` vs Rust build on a local HTTP server).
Consequences: any struct/field change is possible during development
("modifikasi kode atau infrastruktur") to reach the speed goal — recorded
cases (app-cases) must still pass; intentional behavior changes need approval.

## ADR-007: Cutover
Context: User: "Build to build/odm, leave original (Recommended)."
Decision: After every wave + at the end: `build/odm` = static Rust binary;
`./odm` (Go) untouched as reference until user says otherwise. Speed comparison
reported per wave; reshape/verify offer after lift (whole-app.md step 7).
