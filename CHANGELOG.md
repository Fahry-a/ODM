# Changelog

All notable changes to ODM (Oryn Download Manager) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
(pre-1.0.0 versions may evolve without full backward-compat guarantees).

The PRD section references below point at `PRD.md`.

## [Unreleased]

### Added

### Fixed

### Removed

---

## [0.3.0] - 2026-07-26

Colored ILoveCandy UI, transport-level timeout, and connection allocation fixes.

### Added
- ANSI colors to pacman bar: face (`c`/`C`) → yellow, dashes → green, dots →
  cyan. Connections `[xN]` → magenta (active), green (completed), grey (queued).
  Percentage → state-colored. Summary line: green total, yellow speed.
  (`internal/ui/render.go`)
- Colorized confirmation prompt: labels yellow, filename cyan, size green,
  connections magenta, "Continue?" highlighted. Auto-detects TTY.
  (`internal/ui/confirm.go`)
- ANSI-aware truncation (`ansiVisibleWidth`, `truncateVisibleWidth`) so colored
  strings survive terminal width limits without garbled escape sequences.
  (`internal/ui/render.go`)
- Progress bar in summary line with fixed-width columns.
  (`internal/ui/render.go` `RenderSummary`)
- Probe timeout: 15 seconds per URL so a slow/unresponsive server can't block
  the whole batch. (`cmd/odm/main.go`)
- `.odm` control file persisted on every chunk completion (interval 5→1) for
  maximum resume safety. (`internal/download/task.go`)

### Fixed
- Connection allocation: scheduler now uses the Balancer's per-file allocation
  (`a.Connections`) instead of the TaskMaker's global `-c` value. Mode B
  correctly gives 1 conn/file; Mode C distributes remainder correctly.
  (`internal/scheduler/queue.go`)
- Task list ordering race: `emit()` and `LiveViews()` now sort the live slice
  by `TaskID` so task lines don't shuffle between frames.
  (`internal/scheduler/queue.go`)
- Zombie tasks: `isActive()` hides tasks with 0 bytes and 0 speed from the
  display. (`internal/ui/progress.go`)
- Confirmation prompt shows actual balancer allocation, not just the `-sf` flag
  value (which can differ due to remainder distribution).
  (`cmd/odm/main.go`)
- Transport-level timeout: `DialContext` + `TLSHandshakeTimeout` on the
  `http.Transport` instead of per-request `context.WithTimeout`. Body reads
  now run under the task-level context so slow-but-alive streams survive
  indefinitely (aria2c-class behaviour). (`internal/transport/transport.go`,
  `internal/download/task.go`)
- "Total: 0/0" no longer shown before any snapshots arrive.
  (`internal/ui/progress.go`)

### Changed
- PKGBUILD converted to `-bin` package: downloads pre-built binary from
  GitHub Releases instead of building from source. (`packaging/PKGBUILD`)

## [0.2.0] - 2026-07-26 (pre-release)

Enhanced control file, periodic resume checkpoints, CI lint, and test coverage.

### Added
- Tests for `internal/storage` (previously 0): concurrent `WriteAt` non-overlap
  stress, pre-allocation, sizeless stream, resume control file round-trip,
  missing/corrupt control file, atomic save, idempotent remove, stray `.tmp`
  sweep. (`internal/storage/file_test.go`, `internal/storage/resume_test.go`)
- Tests for `internal/logging` (previously 0): level filtering ladder, format
  passthrough, file mirror, nil-safety, `TaskLogFn` level mapping.
  (`internal/logging/logging_test.go`)
- golangci-lint v2 config with `unconvert` enabled; `errcheck` excludes
  `defer x.Close()` and `fmt.Fprint*`; ST1012 disabled for `NoControlFile`.
  CI now runs `golangci-lint` between vet and test. (`.golangci.yml`,
  `.github/workflows/ci.yml`)
- `CHANGELOG.md`.
- Enhanced `.odm` control file with richer metadata: `created_at`/`updated_at`
  timestamps, `connections` count, `user_agent`, `odm_version`, and `checksum`
  (if `--checksum` was used). New helper methods `BytesDone()`, `FractionDone()`,
  `Age()`, and `Summary()` on `ControlFile` for diagnostics. All new fields are
  `omitempty` for backward compatibility with v0.1.0 files.
  (`internal/storage/resume.go`)

### Fixed
- `storage.File.Close()` is now idempotent (previously returned
  "file already closed" on a second call). `logging.Logger.Close()` likewise.
- Removed unnecessary `io.ReadCloser()` conversions in `task.go`
  (`rr.Resp.Body` and `resp.Body` are already `io.ReadCloser`).
- Control file (`.odm`) is now persisted periodically during download
  (every 5 completed chunks) instead of only at task finish. A crashed or
  killed process now leaves a usable resume point, matching aria2's
  `.aria2` checkpoint behaviour. (`internal/download/task.go`)

### Removed
- Dead code: `ChunkQueue.taskDone` field, `Task.finished` field,
  `Manager.formatDuration` func, `Limiter.allowN` method, `tickRate` const,
  `readEvents` test helper. None were referenced anywhere.

---

## [0.1.0] - 2026-07-25 (pre-release)

The initial MVP — a single-binary CLI download manager inspired by `aria2c`,
implementing the full `PRD.md` specification (v1.4). Built around three core
features: the Connection Balancer, the pacman/ILoveCandy progress bar, and a
JSON-RPC + WebSocket control surface.

### Added

**Connection Balancer (PRD §5)**
- Mode A (single file): whole budget → one file, `min(-c, --max-connections)`.
  (`internal/scheduler/balancer.go:125`, `TestModeA_AllBudgetToSingleFile`)
- Mode B (batch, no `-sf`): `-c` controls files in parallel, 1 connection each.
  (`internal/scheduler/balancer.go:140`, `TestModeB_FilesInParallelQueued`)
- Mode C (batch with `-sf N`): `floor(-c / N)` files in parallel, remainder
  distributed one-at-a-time to the first files. (`balancer.go:163`,
  `TestModeC_PRDExample`, `TestModeC_RemainderDistribution`)
- Allocation-time reallocation: files failing the range probe are capped to 1
  connection; freed budget redistributed to siblings in the same scheduling
  pass. (`balancer.go:191`, `TestAllocationTimeReallocation`)
- Probe 3-step chain (HEAD → ranged `GET bytes=0-0` → plain GET fallback) for
  range support + Content-Length discovery. (`internal/transport/transport.go:178`)
- Validation: `C<1` error, `C>MaxConnections` warning, `SF>C` error, `-sf`
  silently ignored when `N==1`. (`balancer.go:79-107`)

**Download engine (PRD §11)**
- Work-stealing chunk queue (per-task queue, workers pull next chunk): avoids
  straggler problem of static equal-split. (`internal/download/chunkqueue.go`,
  `internal/download/task.go`, `TestWorkStealing_BeatsStaticEqualSplit`)
- Single-stream fallback for files that don't support ranges (degenerate
  queue of one full-file chunk). (`task.go` `fetchWhole`)
- Resume via `.odm` control file: records URL / total size / chunk size /
  completed chunk offsets, re-queues only un-done chunks, deleted on clean
  completion. (`internal/storage/resume.go`, `TestManager_ResumeInterrupted`)
- Global token-bucket rate limiter shared across all workers, throttled at
  the stream/read level. (`internal/ratelimit/bucket.go`,
  `TestLimitRate_StableAggregate`)
- Retry/backoff per chunk, configurable via `--retry` and `--retry-wait`.
  (`task.go:401-405`)
- Pre-allocation via `os.Truncate` to Content-Length; concurrent `WriteAt` for
  chunk writes (boundaries never overlap → no lock needed). (`storage/file.go`)

**CLI + configuration (PRD §6, §7)**
- All 30+ CLI flags from PRD §6.2 wired: connections, max-connections,
  split-file, output, dir, input-file, yes, quiet, continue, chunk-size,
  max-redirect, retry, retry-wait, timeout, user-agent, header, referer,
  proxy, check-certificate, checksum, limit-rate, config, log, log-level,
  rpc, rpc-listen-port, rpc-listen-all, rpc-secret, version, help.
  (`internal/config/config.go:295` `BindFlags`)
- Batch URL input: space-separated positional args (recommended), legacy
  comma-joined single arg (preserved for backward compat, with the
  `commaFollowedByScheme` heuristic so comma-in-URL survives), and
  `-i <file>` (one URL per line, `#` comments + blanks skipped).
  (`internal/config/load.go:128` `resolveURLs`, `TestSetup_BatchURLParsing_Integration`)
- Config file layering: CLI > `~/.config/odm/config.conf` >
  `/etc/odm/config.conf` > defaults; `key = value` format, `#` comments,
  unknown keys silently skipped (forward-compat), inline comments stripped.
  A defaulted CLI flag never overwrites a value the user put in a file.
  (`internal/config/config.go:160` `Parse`, `load.go:71` `LoadLayers`,
  `TestSetup_LayeredConfigMergedWithURLs`)
- `-sf` is rewritten to `--split-file` by `NormalizeArgs` because pflag
  shorthand must be a single char. (`config.go:341`)
- Reference config template: `configs/odm.conf.example`.

**Progress bar — pacman / CachyOS ILoveCandy style (PRD §8)**
- Pure rendering layer (testable, no I/O): `Bar()` and `BarIndeterminate()`
  with the `c`/`C` face animation (`internal/ui/render.go`).
- Stateful `Renderer`: per-task cursor cache, live TTY redraw loop at ~100 ms
  cadence, non-TTY throttled log fallback (~2 s + completion-milestone flush).
  (`internal/ui/progress.go`)
- `[x<N>]` per-file connection indicator. (`render.go:302`)
- Fixed column layout (`BarWidth=30`, `colSize=9`, `colSpeed=11`, `colETA=8`,
  `colConns=5`) — pinned by `TestRenderTaskLine_FixedColumns`.
- Color states: green=completed, yellow=active, red=retrying/error, grey=
  queued/paused; auto-disabled on non-TTY or `NO_COLOR`. (`render.go:
  stateColor`, `progress.go:35` `shouldColor`)
- ETA `HH:MM:SS` (8 cells), capped at `99:59:59`. (`render.go:126`)
- Summary line `Total: X/Y completed | <speed>/s | ETA …`.
  (`render.go:328` `RenderSummary`)
- Terminal width detection via raw `ioctl(TIOCGWINSZ)` on Linux (no
  external lib dependency), 80-col fallback on other platforms.

**Confirmation prompt (PRD §9)**
- Single-file and batch layouts, Y/n with re-prompt on bad input, EOF =
  silent cancel. (`internal/ui/confirm.go`)
- Skipped by `-y`/`--yes`, `--quiet`, or in `--rpc` daemon mode.
  (`cmd/odm/main.go:143`)

**RPC server — JSON-RPC 2.0 + WebSocket (PRD §10)**
- JSON-RPC 2.0 over HTTP POST at `/rpc`; WebSocket fan-out at `/ws`.
  (`internal/rpc/server.go`, `internal/rpc/ws.go`)
- All 14 PRD §10.2 methods registered: `addUri`, `addBatch`, `pause`,
  `pauseAll`, `unpause`, `unpauseAll`, `remove`, `tellStatus`, `tellActive`,
  `tellWaiting`, `tellStopped`, `changeOption`, `getGlobalStat`,
  `getVersion`, `shutdown`.
- All 5 §10.3 events emitted: `onDownloadStart`, `onDownloadProgress`
  (~250 ms throttled), `onDownloadComplete`, `onDownloadError`,
  `onDownloadPause`.
- aria2-style auth: first param `"token:<secret>"` (or `?secret=` on the
  WS upgrade). Default bind `127.0.0.1`; `--rpc-listen-all` binds
  `0.0.0.0` (should be paired with `--rpc-secret`).
- `changeOption` is honoured as acknowledged no-op in MVP — mid-flight
  option mutation is a §15 roadmap item. (`server.go:187`, acknowledged in
  README)

**Reliability**
- Checksum verification (md5/sha1/sha256) for single-file downloads.
  (`internal/download/manager.go:151` `VerifyChecksum`)
- Leveled logger with optional file mirror (`--log` / `--log-level`).
  (`internal/logging/logging.go`)
- Exit codes 0/1/2/3/4 per PRD §13. (`manager.go` `ExitOK`, `ExitGeneral`,
  `ExitNetwork`, `ExitPartial`, `ExitCancelled`, `ExitCodeFrom`)

**Packaging + tooling**
- SPDX-MIT licensed.
- `packaging/PKGBUILD` for Arch/CachyOS.
- `docs/odm.1` man page.
- GitHub Actions CI: `go mod download` → `go build` → `go vet` →
  `go test -race -count=1`. (`.github/workflows/ci.yml`)
- `AGENTS.md` onboarding doc for AI coding agents.
- Version tagged `v0.1.0`.

### Fixed
- Real-time ETA, per-file `[x<N>]` connection count, spaced pacman dots
  (`o o o`), and the `c` ↔ `C` face animation. (`abbd327`)
- Terminal progress snapshot emitted before `Task.Start` returns so the UI
  doesn't briefly render an empty state on completion. (`3b0419a`)
- PKGBUILD cleanups (`36761b5`), `.gitignore` tidy (`ca412ae`).

### Security
- TLS certificate verification enabled by default (`--check-certificate=true`).
  (`internal/transport/transport.go:68`)
- RPC server binds to `127.0.0.1` only by default; `--rpc-listen-all`
  documented to require `--rpc-secret`. (`cmd/odm/main.go:368`,
  `internal/rpc/server.go:90`)
- `/etc/odm/config.conf` documented as root-owned `0644`.

### Known limitations (per PRD §15, deferred to roadmap)
- Mid-flight dynamic reallocation of already-downloading chunks.
- Multi-mirror download across duplicate URLs.
- HTTP/2 / HTTP/3 stream multiplexing (deliberately out of scope for the
  connection-aggregation value proposition).
- Per-task speed limits (only the global `--limit-rate` exists today).
- BitTorrent / magnet links.
- No dedicated WebSocket test asserting the `onDownloadPause` event (the
  emit path is exercised via `odm.pause`, but not received-end-to-end in
  a test).

[Unreleased]: https://github.com/Fahry-a/ODM/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/Fahry-a/ODM/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Fahry-a/ODM/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Fahry-a/ODM/releases/tag/v0.1.0
