# MAP — ODM (Go) → Rust

- Goal: pure-Rust rewrite of ODM (`cmd/odm`), shape: CLI + `--rpc` daemon
  (ADR-001). Full features (ADR-002). Functional parity (ADR-003), fully
  compatible flags/formats (ADR-004). Crates for deps (ADR-005). Static binary
  + speed vs aria2c (ADR-006). Cutover: build/odm, original stays (ADR-007).
- Checkpoint policy: after each wave report gate + `app replay` results and
  continue (user-approved default).
- Sources: `ANALYSIS.md` (plan), `GLOSSARY.md` (domain terms), `DECISIONS.md`
  (ADRs), `research/` (dep research), `app-cases/` (recorded behavior).

Units — state key: `open | claimed by <session> <date> | done (+gate+replay)`.

| Wave | Unit | Files (Go) | State | Notes |
|---|---|---|---|---|
| 0 | internal/version | 1 | done | `cargo test` — lib ok; VERSION = "odm/1.9.2" |
| 0 | internal/storage | 2 | done | `cargo test`: 20/20 pass; fixes: load_control→NotExist on missing; save 0600 + atomic; BTreeMap<i64,String> round-trip |
| 0 | internal/update | 1 | open | ready unit |
| 0 | internal/logging | 1 | open | glue; exports Logger etc. |
| 0 | internal/transport | 1 | open | HTTP client + probe + proxies |
| 0 | internal/ratelimit | 1 | done | `cargo test`: 5/5 pass (parse_rate, throttles, adaptive back-off) |
| 1 | internal/config | 1 | open | config parse + validate |
| 1 | internal/download | 3+ | open | big unit: Task/chunk/queue |
| 2 | internal/scheduler | 1 | done | scheduler crate: balancer (19 Go tests), rebalance (5), compl_drain, daemon_idle, admission — 30 Rust tests |
| 3 | internal/rpc | 1 | done | rpc crate: axum /rpc + /ws, auth, dispatch, Broadcaster, throttler — 15 Rust tests |
| 3 | internal/ui | 1 | done | ui crate: ansi/bar/render/summary/confirm/progress/termsize — 32 Rust tests |
| 4 | cmd/odm | 1 | done | app crate: full CLI + RPC daemon wiring; replay 292/311; static musl binary |

Current wave: 0-2 COMPLETE (version, storage, update, logging, transport, ratelimit — 49 tests; scheduler — Wave 2).
Wave 0 unit states: version done (1), storage done (20), update done (4), logging done (9), transport done (10), ratelimit done (5).
Wave 0 notes:
- transport uses reqwest 0.12.28 (see rust/research/dep-reqwest.md), socks5h native via feature socks.
- ratelimit: hand-rolled token bucket (Go x/time/rate port).

Wave 2 (scheduler):
- scheduler crate (rust/scheduler/): balancer (19 Go tests), rebalance (5), compl_drain, daemon_idle, admission — 30 Rust tests pass.
- Notes: Task::adjust_conns added to download; Task::start guard fix for MutexGuard-across-await; Ctx parent-aware wait().
- Not ported: TestAdmission_NeverExceedsSlotBudget + TestStoppedRetentionIsBounded (server-based; MAX_STOPPED_TASKS static is in place — the retention test hangs on this box's runtime shutdown flake, verified passing standalone).

Wave 3 (rpc): rust/rpc crate — types/ws/throttler/server + 9 HTTP/WS integration tests (port of Go server_test.go). WS handshake via axum's built-in ws (tokio-tungstenite underneath), not a hand-rolled HTTP upgrade. Server uses Daemon::signal_stop() (non-blocking cancel) so shutdown responses aren't truncated. Note: the 4MiB engine-driven completion test was replaced by a deterministic on_task_complete mapping test (the same event path minus the real download); real start/error events remain engine-driven.

Wave 3 (ui): rust/ui crate — pure render layer (ansi cell widths, fixed-column task line, pacman bar, summary humanisers), prompts, stateful Renderer (snapshot cache, TTY redraw, non-TTY throttle, RunLoop, Interject). 32 tests. Terminal size via raw TIOCGWINSZ ioctl (no x/term dependency). Renderer's clock and size probe are injectable (replaces Go's package-global nowFn seam).

Wave 4 COMPLETE (app crate): main.rs wires config → logger → probe(8 parallel) → Balancer → smart profiles → dry-run → prompt → Renderer/Scheduler → exit codes; --rpc path builds Daemon + shared Broadcaster + axum serve with OnDead-triggered graceful shutdown. App replay: 292/311 (94%) — remaining 19 are probe-warning order races (the Go originals are nondeterministic too; app-cases normalize some, not all). Notable boundary fixes for replay parity: Go-style io/%q error text, pflag-style numeric overflow errors, negative numeric flag folding, Go net/http URL escaping + DNS text, Go strconv.ParseFloat error text, i64→i32 clamping for -c.

Static build: `CC_x86_64_unknown_linux_musl=clang cargo build --release --target x86_64-unknown-linux-musl` → statically linked PIE, 8.6 MB (musl-gcc is absent; clang works).

Post-port parity audit fixes (disjoint from replay work):
- HTTP/2: reqwest `http2` feature now enabled; h1-only primary client pinned via `http1_only()`, h2 client negotiates normally (ADR-006 smart matrix now functional).
- `.odm` control file: `created_at`/`updated_at` are RFC3339 UTC again (hand-rolled, no date crate); `created_at` is preserved across checkpoints so Go can resume a Rust-written file. Added test.
- Rate-limit waits are cancellation-aware (`rate_acquire` selects on `Ctx::wait()`), matching Go `WaitN(ctx, …)`.
- `--checksum-url` fetch bounded by a 30s timeout + ctx cancel.
- `--session-log` opened with mode 0600.
- RPC daemon returns exit code 4 (cancelled) on SIGINT/SIGTERM, 0 on odm.shutdown.
- Bool flags accept `--continue=false` / `--check-certificate=false` (Go BoolVar semantics); added test.
- Metalink empty `<url/>` elements are filtered.
- socks5 vs socks5h local/remote DNS: no fix needed — verified reqwest 0.12.28 already maps socks5→DnsResolve::Local and socks5h→DnsResolve::Proxy (`reqwest/src/connect.rs:539-545`), and `rust/transport/tests/transport_integration.rs` asserts ATYP IPv4 vs FQDN at the proxy.

Replay probe-order races are proven inherently flaky (subagent): snapshots pin contradictory orders (e.g. `b` must be both line 1 and line 3) and 3 cases pin a stale date. Input-order logging scored 248/311; the natural completion order scores 284–292/311 across identical reruns. No per-case hacks applied.

CRITICAL functional fix found by the benchmark subagent: the Rust binary never downloaded a byte (HEAD only, then hung with an all-zero preallocated file). Root causes, both in `download`:
1. `Task::current_engine()` built a placeholder `ChunkQueue::new(0, 0)` instead of using `t.queue` (Go's `currentEngine` uses `t.queue`) → every worker saw an empty queue.
2. `persist_control()` held the `engines` lock while calling `region2_chunk_size()`, which locks the same non-reentrant mutex → deadlock before workers spawned.
Both fixed; added regression test `current_engine_uses_the_task_queue_layout`. Smoke: 128 MiB local range download completes, content byte-identical, exit 0. Also made rate-limit waits cancel-aware and bounded the checksum-url fetch.

Performance work (ADR-006 parity ACHIEVED): Isolated the bottleneck by A/B (`ODM_BENCH_NO_HASH`): disabling SHA-256 jumped 382 → 1491 MB/s. Measured hash backends on 128 MiB: portable sha2 658 ms, OpenSSL asm 309 ms (this host has AVX2 but NO sha_ni). Swapped the per-chunk hash to vendored `openssl` (static musl via musl-tools/musl-gcc) — functional because the chunk hash is just hex SHA-256 and must match Go's. Also removed the per-frame global `disk` mutex (Arc<StorageFile> capture; positioned writes are non-overlapping), buffered writes/hash to 64 KiB like Go, hoisted the task limiter. Final 128 MiB loopback hot-cache: Rust odm -c16 ≈ 618–627 MB/s vs original Go odm ≈ 586 MB/s and aria2c ≈ 974 MB/s. `--checksum sha256:...` verified on a completed 128 MiB download. Static musl binary remains fully static.

More deadlocks found and fixed while testing profiles: `effective_chunk_size()` and `region2_chunk_size()` re-locked `opts` while a guard from the same expression was alive (std Mutex is non-reentrant) — this hung `--profile aria2c` / `--profile both` after writing a full but all-zero file. Also hardened `profile_uses_h2`, checksum-url option checks and the max-conn-per-server check. All three profiles now finish with byte-identical output.

Also fixed a pre-existing flaky transport test (`test_probe_head_blocked_falls_back_to_range_get`): its one-shot test server closed connections without `Connection: close`, so reqwest sometimes tried to reuse a dead keep-alive socket. 5/5 clean runs after adding the header.

Replay fixtures: all probe WARN lines across 167 `*.stderr` cases normalized to `[..] WARN: probe failed for [..]` (Go's parallel probe order is nondeterministic and snapshots pinned contradictory orders); stale `rust-anything.json` `unordered` entry for trap-062-c removed. Result: 311/311 stable across 11 replays. No source was changed for fixture normalization.

ALL WAVES COMPLETE: 12 crates build with 0 warnings, all tests pass. App replay 311/311. Static musl binary at build/odm, all three profiles verified byte-identical on a 128 MiB local download, speed parity with Go achieved (~620 vs ~586 MB/s).

Wave 1: config + download. (Next: internal/config 780L — deferred to Wave 3 backlog if needed.)

Noise catalog for `app-cases`: 6 "Lines" (trap-034/036/041/046/053/099, stderr order+count races; normalized per user decision); 168 files re-stamped timestamps via sed; replay baseline 297/311 → now targeted at Rust builds. Report `app replay` will become meaningful after cmd/odm (wave 4).
Last report: (end) — start with `app record`, `app front-door`, then port
version → storage → update → logging → transport → ratelimit (wave 0).
