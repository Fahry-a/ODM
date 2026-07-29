# Changelog

All notable changes to ODM (Oryn Download Manager) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.1.0] - 2026-07-29

> **Dynamic rate limiting, per-task speed caps, mid-flight connection reallocation,
> hybrid BytesDone/Total progress bar.**
>
> `changeOption` is now a real mutation endpoint. `--limit-rate-per-task` adds
> a per-task token bucket stacked on the global one. Workers can be added or
> removed mid-flight with graceful drain. The progress bar now shows live
> downloaded/total sizes instead of a static total.

### Added
- **Dynamic rate limit update** — `Limiter.SetRate(spec)` updates the global
  token bucket at runtime via RPC `odm.changeOption` key `max-download-limit`.
  (`internal/ratelimit/bucket.go`, `internal/rpc/server.go`)
- **Per-task speed limits** — new flag `--limit-rate-per-task` creates a
  per-task token bucket stacked on the global one. The body is throttled
  through both, so a task caps at `min(per-task, global)` and the aggregate
  never exceeds the global cap. RPC key `max-download-limit-per-task`.
  (`internal/config/config.go`, `internal/download/task.go`, `cmd/odm/main.go`)
- **Mid-flight connection reallocation** — `Task.AdjustConns(target)` changes
  the desired connection count at runtime. Reduction: excess workers check
  `connTarget` before pulling the next chunk and retire gracefully. Increase:
  new worker goroutines are spawned. RPC key `connections`.
  (`internal/download/task.go`, `internal/scheduler/daemon.go`)
- **Hybrid BytesDone/Total column** — the per-file size column now shows
  `42.0M/256.0M` (downloaded/total) using compact single-letter suffixes,
  growing live during download. (`internal/ui/render.go`)

### Changed
- **`odm.changeOption`** is no longer a no-op — supports `max-download-limit`,
  `max-download-limit-per-task`, and `connections` keys.
- **Progress bar layout**: `colSize` increased from 9 to 14 to accommodate
  the hybrid done/total format. Summary line now shows aggregate bytes.
- **Man page and README** updated to document new flags and RPC keys.

## [1.0.1] - 2026-07-28

> **Short flag aliases, Content-Disposition parsing, and UI polish.**
>
> Adds short CLI flags for common options, extracts filenames from
> `Content-Disposition` headers, fixes the pacman dot-shifting bug,
> and improves crash resilience with frequent control-file persistence.

### Added
- **Short flag aliases** for 11 flags: `-m` (max-connections), `-n` (max-redirect),
  `-r` (retry), `-w` (retry-wait), `-t` (timeout), `-u` (user-agent), `-p` (proxy),
  `-l` (limit-rate), `-s` (chunk-size), `-L` (log), `-V` → `-v` for version
  (uppercase `-V` preserved for backward compatibility).
  (`internal/config/config.go`, `cmd/odm/main.go`, `docs/odm.1`)
- **Content-Disposition header parsing** — probes now extract the `filename`
  parameter from `Content-Disposition` headers per RFC 6266, supporting
  both `filename=` and `filename*=UTF-8''...` forms.
  (`internal/transport/transport.go`)
- **Crash-resilient control-file persistence** — control file is flushed on
  every chunk error path, so partial progress survives process termination
  before the task's main error handler runs.
  (`internal/download/task.go`)

### Changed
- **Progress bar dots anchored to absolute positions** — the alternating
  `o`/` ` pattern is now computed from the cell's absolute position instead
  of relative to the pacman face, so dots stay fixed and don't shift right
  as pacman advances. (`internal/ui/render.go`)
- **Connection indicator moved outside bar brackets** — `[xN]` is now shown
  before the bar `[...]` instead of inside it, with its own magenta/grey
  coloring independent of the pacman bar colors.
  (`internal/ui/render.go`)
- **Checkpoint interval lowered to 1** — control file is persisted on every
  chunk completion (was every 5 chunks), maximising resume granularity at
  negligible I/O cost. (`internal/download/task.go`)
- **Scheduler drain wait** — `Run()` now waits up to 200ms on cancellation
  for in-flight workers to persist control files before returning.
  (`internal/scheduler/queue.go`)
- `--chunk-size` now has `-s`, `--max-connections` has `-m`, `--timeout`
  has `-t`, `--retry` has `-r`, `--proxy` has `-p`, `--user-agent` has `-u`,
  `--limit-rate` has `-l`, `--retry-wait` has `-w`, `--max-redirect` has `-n`,
  and `--log` has `-L`.

### Fixed
- **Pacman dots shifting right** — the remaining-dot pattern after the face
  no longer recomputes from a fresh alternating layout every frame; dots
  stay at their fixed column positions and get eaten one by one as the face
  advances. (`internal/ui/render.go`)

---

## [1.0.0] - 2026-07-27

> **Production-ready release — TLS, systemd, ETag validation, final UI polish.**
>
> All roadmap items from AGENTS.md are implemented. The CLI, RPC daemon,
> progress bar, and resume engine are stable and tested under -race.

### Added
- **TLS for RPC** — `--rpc-tls-cert` and `--rpc-tls-key` flags enable HTTPS/WSS on the RPC listener, with `http.ServeTLS`. Both must be provided together. (`internal/config/config.go`, `cmd/odm/main.go`)
- **ETag validation on resume** — on `--continue`, the stored ETag from the `.odm` control file is compared against the server's current ETag. If both are non-empty and don't match, the file is re-downloaded from scratch to prevent corruption. (`internal/download/task.go`)
- **systemd service unit** — `packaging/odm.service` with security hardening: `DynamicUser=yes`, `ProtectSystem=strict`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, and `EnvironmentFile` support for `/etc/odm/odm.env`. (`packaging/odm.service`)
- Dependabot configuration for automated dependency updates (`go.mod` + GitHub Actions).

### Changed
- **Pacman-style progress bar** — connection count `[xN]` indicator moved inside the bar brackets (e.g. `[x4---c  o  o  o]`) instead of a separate column. On TTY, the name field expands dynamically to fill terminal width so the info block (size/speed/ETA/bar/%) sits at the right edge of the terminal, matching the ILoveCandy layout. Summary bar width increased from 20 to 30 for consistency; summary info block is right-aligned to terminal width. (`internal/ui/render.go`, `internal/ui/progress.go`)
- **Checkpoint interval**: `persistCheckpointInterval` raised from 1 to 5 — reduces `.odm` JSON write frequency from per-chunk (potentially hundreds/sec) to every 5 chunks, with no material impact on crash recovery (at most ~20 MB lost). (`internal/download/task.go`)
- `actions/setup-go` upgraded from v5 to v7 in CI workflows.

### Fixed
- **Memory leak**: removed unused `Daemon.pending` slice that grew unboundedly. (`internal/scheduler/daemon.go`)
- **Context leak**: `Enqueue` now accepts a context (from the daemon) instead of `context.Background()`, so queued tasks respect daemon shutdown. (`internal/scheduler/queue.go`)
- **Stalled connection hang**: per-chunk timeout (`Timeout × 10`, default 300s) wraps `fetchAndWrite` and `fetchWhole` so a dead connection eventually retries instead of blocking forever. `ResponseHeaderTimeout` added to the HTTP transport. (`internal/download/task.go`, `internal/transport/transport.go`)
- **Pause race**: removed the spurious `pauseC` drain at the top of the worker loop — the drain could consume the unpause signal before the paused check, causing a worker to block forever. (`internal/download/task.go`)
- **Empty-file `Done()`**: `ChunkQueue.Done()` now returns true when the queue is empty even if no chunks were ever completed (0-byte files). (`internal/download/chunkqueue.go`)
- **Dead parameter**: removed unused 4th parameter from `distribute()`. (`internal/scheduler/balancer.go`)

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

[Unreleased]: https://github.com/Fahry-a/ODM/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/Fahry-a/ODM/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/Fahry-a/ODM/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/Fahry-a/ODM/compare/v0.3.0...v1.0.0
[0.3.0]: https://github.com/Fahry-a/ODM/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Fahry-a/ODM/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Fahry-a/ODM/releases/tag/v0.1.0
