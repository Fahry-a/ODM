# GLOSSARY — ODM (Go) → Rust names

Domain terms of the original and their names in the Rust port (GLOSSARY.md,
`rust/` workspace layout). Add/edit as units bring new terms.

- **Task** (download job): `download::Task` — internal/download/task.go.
  Fields: url, output, dir, conns, state (Running/Stopped/...), size, done.
- **Chunk** (fixed-size segment): `download::Chunk` — internal/download/chunk.go.
  Default size 4M (`-s/--chunk-size`).
- **Work-stealing** (chunk queue): `download::WorkStealingQueue` —
  internal/download/queue.go. Free workers pull the next chunk.
- **Connection Balancer**: `scheduler::ConnectionBalancer` — internal/scheduler.
  Modes A/B/C (allocation modes) — `AdjustConns` mid-flight per roadmap.
- **Control file** (`.odm`): `storage::ControlFile` — internal/storage/resume.go.
  Per-chunk hashes stored for resume integrity (ADR-004 compatibility).
- **Probe**: `transport::Probe` — internal/transport. HEAD → ranged GET →
  single-stream fallback.
- **Profile** (`--profile aria2c|both|smart`): `download::EngineProfile` /
  `scheduler` — engine profiles (odm default).
- **Mirrors**: alternative URLs serving the same file; chunks rotate between
  mirrors (`-l/--mirror`? actually `--mirror` per help: `--mirror URL`).
- **Session log** (`-s/--session-log`): JSONL events written by `ui`/download
  for wrappers/GUIs.
- **Metalink4** (`.meta4`): multi-file metalink input via `-i`.
- **Connection budget**: `-c/--connections` total budget; `-m/--max-connections`
  ceiling (warn when exceeded).
- **Self-update** (`odm update`): `update::` — internal/update/update.go.
- **RPC daemon** (`--rpc`): `rpc::` — internal/rpc: JSON-RPC 2.0 + WebSocket
  server, `Broadcaster`, `getVersion`, `addURIAndTellActive` etc.
- **Dry-run** (`--dry-run`): probe URLs + show plan, download nothing.
- **skip-existing / auto-rename**: `internal/download` — existing-file policy.
