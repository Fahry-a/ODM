# Changelog

All notable changes to ODM (Oryn Download Manager) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
(pre-1.0.0 versions may evolve without full backward-compat guarantees).

## [Unreleased]

### Added
- Dependabot configuration for automated dependency updates (`go.mod` + GitHub Actions).

### Changed
- `actions/setup-go` upgraded from v5 to v7 in CI workflows.

### Removed
- `PRD.md` — migrated remaining roadmap items to `AGENTS.md`.

---

## [0.3.0] - 2026-07-26

> **Colored ILoveCandy UI, transport-level timeout, and connection allocation fixes.**

### Highlights

- **ANSI color support** for the pacman progress bar — face, dots, connection indicator, and summary line are all color-coded by state.
- **Transport-level timeouts** — `DialContext` + `TLSHandshakeTimeout` replace per-request timeouts; slow-but-alive streams survive indefinitely (aria2c-class behavior).
- **Connection allocation fix** — scheduler now uses the Balancer's per-file allocation instead of the global `-c` value, fixing incorrect connection counts in Mode B and Mode C.

### Added
- ANSI colors on pacman bar: face (`c`/`C`) → yellow, dashes → green, dots → cyan. Connection indicator `[xN]` → magenta (active), green (completed), grey (queued). Percentage → state-colored. Summary: green total, yellow speed. (`internal/ui/render.go`)
- Colorized confirmation prompt with TTY auto-detection. (`internal/ui/confirm.go`)
- ANSI-aware truncation (`ansiVisibleWidth`, `truncateVisibleWidth`) for safe terminal width limits. (`internal/ui/render.go`)
- Progress bar in summary line with fixed-width columns. (`internal/ui/render.go` `RenderSummary`)
- Probe timeout: 15 s per URL so a slow server can't block the batch. (`cmd/odm/main.go`)
- `.odm` control file persisted on every chunk completion (interval 5→1). (`internal/download/task.go`)

### Fixed
- Connection allocation: scheduler uses Balancer's per-file `a.Connections` instead of global `-c`. Mode B gives 1 conn/file; Mode C distributes remainder correctly. (`internal/scheduler/queue.go`)
- Task list ordering race: `emit()` and `LiveViews()` sort by `TaskID` to prevent line shuffling. (`internal/scheduler/queue.go`)
- Zombie tasks: `isActive()` hides tasks with 0 bytes and 0 speed. (`internal/ui/progress.go`)
- Confirmation prompt shows actual balancer allocation, not just `-sf` flag value. (`cmd/odm/main.go`)
- Transport timeout: `DialContext` + `TLSHandshakeTimeout` on `http.Transport` instead of per-request `context.WithTimeout`. (`internal/transport/transport.go`, `internal/download/task.go`)
- "Total: 0/0" no longer shown before snapshots arrive. (`internal/ui/progress.go`)

### Changed
- PKGBUILD converted to `-bin` package: downloads pre-built binary from GitHub Releases. (`packaging/PKGBUILD`)

---

## [0.2.0] - 2026-07-26

> **Enhanced control file, periodic resume checkpoints, CI lint, and test coverage.**

### Highlights

- **Richer `.odm` control files** — now include timestamps, connection count, user agent, ODM version, and checksum metadata (all `omitempty` for backward compatibility).
- **Periodic resume checkpoints** — control file saved every 5 chunks instead of only at task finish, matching aria2's `.aria2` behavior.
- **golangci-lint v2** integrated into CI with custom config for this project.

### Added
- Tests for `internal/storage`: concurrent `WriteAt` non-overlap stress, pre-allocation, resume round-trip, missing/corrupt control file, atomic save, stray `.tmp` sweep. (`internal/storage/file_test.go`, `internal/storage/resume_test.go`)
- Tests for `internal/logging`: level filtering ladder, format passthrough, file mirror, nil-safety, `TaskLogFn` level mapping. (`internal/logging/logging_test.go`)
- golangci-lint v2 config; CI runs `golangci-lint` between vet and test. (`.golangci.yml`, `.github/workflows/ci.yml`)
- `CHANGELOG.md`.
- Enhanced `.odm` control file: `created_at`/`updated_at` timestamps, `connections`, `user_agent`, `odm_version`, `checksum`. New methods: `BytesDone()`, `FractionDone()`, `Age()`, `Summary()`. (`internal/storage/resume.go`)

### Fixed
- `storage.File.Close()` and `logging.Logger.Close()` are now idempotent.
- Removed unnecessary `io.ReadCloser()` conversions in `task.go`.
- Control file persisted periodically (every 5 chunks) instead of only at finish. (`internal/download/task.go`)

### Removed
- Dead code: `ChunkQueue.taskDone`, `Task.finished`, `Manager.formatDuration`, `Limiter.allowN`, `tickRate`, `readEvents` test helper.

---

## [0.1.0] - 2026-07-25

> **Initial MVP — single-binary CLI download manager inspired by `aria2c`.**
>
> Built around three core features: the Connection Balancer, the pacman/ILoveCandy progress bar, and a JSON-RPC + WebSocket control surface.

### Highlights

- **Connection Balancer** — automatic parallel connection allocation across single-file (Mode A), batch (Mode B), and split-file batch (Mode C) modes.
- **Work-stealing chunk queue** — avoids the straggler problem of static equal-split segmentation.
- **Pacman/CachyOS ILoveCandy progress bar** with `[xN]` connection indicator and ETA.
- **JSON-RPC 2.0 + WebSocket** server with 14 methods and 5 event types.

### Added

**Connection Balancer**
- Mode A (single file): whole budget → one file, `min(-c, --max-connections)`. (`internal/scheduler/balancer.go:125`)
- Mode B (batch, no `-sf`): `-c` controls files in parallel, 1 connection each. (`balancer.go:140`)
- Mode C (batch with `-sf N`): `floor(-c / N)` files in parallel, remainder distributed to first files. (`balancer.go:163`)
- Allocation-time reallocation: files failing range probe capped to 1 conn; freed budget redistributed. (`balancer.go:191`)
- Probe 3-step chain (HEAD → ranged GET → plain GET fallback). (`internal/transport/transport.go:178`)
- Validation: `C<1` error, `C>MaxConnections` warning, `SF>C` error, `-sf` ignored when `N==1`.

**Download engine**
- Work-stealing chunk queue: workers pull next chunk, avoiding straggler problem. (`internal/download/chunkqueue.go`, `internal/download/task.go`)
- Single-stream fallback for non-range servers. (`task.go` `fetchWhole`)
- Resume via `.odm` control file: URL, total size, chunk size, completed offsets. (`internal/storage/resume.go`)
- Global token-bucket rate limiter across all workers. (`internal/ratelimit/bucket.go`)
- Retry/backoff per chunk (`--retry`, `--retry-wait`).
- Pre-allocation via `os.Truncate`; concurrent `WriteAt` for chunk writes. (`storage/file.go`)

**CLI + configuration**
- 30+ CLI flags wired via pflag. (`internal/config/config.go:295`)
- Batch URL input: space-separated positional args (recommended), legacy comma-joined single arg, `-i <file>`. (`internal/config/load.go:128`)
- Config layering: CLI > `~/.config/odm/config.conf` > `/etc/odm/config.conf` > defaults. (`config.go:160`, `load.go:71`)
- Reference config: `configs/odm.conf.example`.

**Progress bar — pacman / CachyOS ILoveCandy**
- Pure rendering: `Bar()`, `BarIndeterminate()`, `c`/`C` face animation. (`internal/ui/render.go`)
- Stateful `Renderer`: cursor cache, TTY redraw ~100 ms, non-TTY log fallback ~2 s. (`internal/ui/progress.go`)
- `[x<N>]` connection indicator, fixed columns (`BarWidth=30`), ETA `HH:MM:SS` capped `99:59:59`.
- Color states: green=completed, yellow=active, red=error, grey=queued; auto-disabled on non-TTY.

**RPC server — JSON-RPC 2.0 + WebSocket**
- 14 methods: `addUri`, `addBatch`, `pause`, `pauseAll`, `unpause`, `unpauseAll`, `remove`, `tellStatus`, `tellActive`, `tellWaiting`, `tellStopped`, `changeOption`, `getGlobalStat`, `getVersion`, `shutdown`.
- 5 events: `onDownloadStart`, `onDownloadProgress`, `onDownloadComplete`, `onDownloadError`, `onDownloadPause`.
- aria2-style auth: `"token:<secret>"`. Default bind `127.0.0.1`.

**Reliability**
- Checksum verification (md5/sha1/sha256).
- Leveled logger with file mirror (`--log` / `--log-level`).
- Exit codes: `0`=ok, `1`=args, `2`=network, `3`=partial, `4`=cancelled.

**Packaging + tooling**
- SPDX-MIT license, `PKGBUILD` for Arch/CachyOS, `docs/odm.1` man page.
- GitHub Actions CI: `go mod download` → `go build` → `go vet` → `go test -race -count=1`.

### Security
- TLS certificate verification enabled by default.
- RPC binds `127.0.0.1` only; `--rpc-listen-all` documented to require `--rpc-secret`.

### Known limitations (deferred to roadmap)
- Mid-flight dynamic reallocation of already-downloading chunks.
- Multi-mirror download across duplicate URLs.
- HTTP/2 / HTTP/3 stream multiplexing (deliberately out of scope).
- Per-task speed limits (only global `--limit-rate` today).
- BitTorrent / magnet links.

[Unreleased]: https://github.com/Fahry-a/ODM/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/Fahry-a/ODM/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Fahry-a/ODM/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Fahry-a/ODM/releases/tag/v0.1.0
