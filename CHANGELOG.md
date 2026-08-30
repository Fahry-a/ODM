# Changelog

All notable changes to ODM (Oryn Download Manager) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.7.3] - 2026-08-30

### Fixed

- **Skip interactive prompt when install script is piped** — when `odm update` is
  invoked via `curl|sh`, stdin is not a tty; the `[y/N]` prompt now auto-proceeds
  in that case with an informational message. (`docs/public/install.sh`)
- **Force Cloudflare DNS (1.1.1.1) in update client for Android/Termux** — Go's
  pure-Go DNS resolver on Android tries `[::1]:53` (IPv6 localhost) which does not
  exist, causing `connection refused` when calling the GitHub API; the API client
  now always dials Cloudflare over UDP. (`internal/update/update.go`)
- **Isolate failed chunk progress rollback** — under concurrent multi-worker
  downloads, a failing chunk could subtract bytes written by other workers via the
  shared atomic counter; rollback now uses a per-attempt byte tracker to prevent
  progress corruption. (`internal/download/task_io.go`)

## [1.7.2] - 2026-08-29

### Fixed

- **Install script: handle `armv8l` architecture** — Android/Termux devices with
  32-bit mode on ARMv8 CPUs (e.g. Samsung A13) reported `armv8l` from
  `uname -m`, which was unrecognized. Now maps to `arm`. (`docs/public/install.sh`)
- **Install script: checksum verification filename mismatch** — tarball was saved
  as `odm.tar.gz` but `checksums.txt` expects `odm_VERSION_OS_ARCH.tar.gz`,
  causing `sha256sum -c` to fail. (`docs/public/install.sh`)
- **`odm update` crash on Termux/Android** — `isAUR()` called
  `exec.LookPath("pacman")` which triggers `faccessat(AT_SYMLINK_NOFOLLOW)`,
  a syscall blocked by Android's seccomp sandbox (SIGSYS). Now checks
  `pacman` existence via `os.Stat` first. (`internal/update`)

## [1.7.1] - 2026-08-29

### Added

- **`odm update` subcommand** — self-update that auto-detects the install method
  (AUR via `pacman -Qi`, self-installed via install.sh, or manual) and dispatches
  to the appropriate update path: AUR prompts for yay/paru, self-installed
  downloads + replaces the binary in-place, manual prints instructions.
  (`internal/update`, `cmd/odm`)
- **Version hint in `odm -h`** — when a newer release exists on GitHub, the help
  output shows `→ update available: vX.Y.Z (odm update)`. Cached 24h via
  `~/.config/odm/.last-update-check` to avoid hitting the API on every `--help`.
  (`internal/update`, `cmd/odm`)
- **One-line install script** (`curl -fsSL https://odm.orynix.id/install.sh | sh`)
  — auto-detects prefix (writable `/usr/local` → system-wide, else `~/.local`),
  installs binary + man page + config, verifies checksum. (`docs/public/install.sh`)

### Changed

- **README rewritten** — cleaner structure with features overview, one-line
  install, installation guides (AUR, pre-built binaries, source, systemd),
  and a more scannable flag reference table. (`README.md`)

### Fixed

- Duplicate doc comments in `task_io.go` and `task_checksum.go` (merge artifact
  from the `task.go` split into 6 files). (`internal/download`)

## [1.7.0] - 2026-08-25

### Fixed

- **`both` profile: region1 now really runs on HTTP/1.1** — the h2-enabled
  client was handed to the whole task, so region1's work-stealing engine
  collapsed onto one multiplexed TCP stream and silently wasted half the
  connection budget. Client routing is now by engine kind: static-split
  (aria2c-model) engines ride h2 when available; fixed-chunk work-stealing
  engines always keep N separate h1 connections — including degraded
  single-region layouts whose profile string still reads `both`.
  (`internal/download`)
- **UI data race between engine logs and the redraw loop** — log lines
  interjected during a live frame could read torn renderer state
  (`cur.live`/`startedAt`) written unlocked by the frame loop; under the Go
  memory model this is a real race even if it never visibly corrupted a
  frame. All last-seen state is now written only under the renderer lock.
  (`internal/ui`)
- **aria2c display cap race** — `Start` published the raw connection budget
  before capping it to the segment count, so UI/RPC pollers could briefly
  see e.g. `[x16]` for a 4-segment file; the cap is computed first and
  stored once. Also de-flaked `TestAria_ConnsDisplayCappedToSplit`, which
  sampled that window ~50% of the time locally. (`internal/download`)

### Changed

- **Development workflow is now TDD** (red → green → refactor), documented
  in AGENTS.md.
- Housekeeping: removed dead code (`ExecOptions.MaxConn`,
  `PermanentError` alias, a duplicated Content-Range verification block),
  resume failures now warn once per task instead of being swallowed, and
  `task.go` split its resume/checkpoint half into `resume_impl.go`
  (movement only).

### Fixed
Results of audit round 2 (1 critical, 4 high — all fixed with regression
tests):

- **Path traversal via server-controlled filename** — a Content-Disposition
  or URL basename containing `../` (or separators) reached `filepath.Join`
  raw, so a hostile server (or an RPC client on `--rpc-listen-all`) could
  write the download body anywhere the process can — including dotfiles in
  the user's home. Filenames are flattened to a single path component before
  joining; explicit `-o` overrides are untouched. (`internal/download`)
- **RPC `odm.shutdown` bypassed auth** — shutdown detection ran on the raw
  request while auth was checked inside dispatch, so an unauthenticated POST
  got an auth-error response *and* still killed the daemon, cancelling all
  in-flight downloads. Detection is now gated on the same secret check.
  (`internal/rpc`)
- **RPC `changeOption connections ≤ 0` caused silent data loss** — target 0
  retired every worker via the graceful drain while chunks remained queued;
  Start reported the half-downloaded file as *completed* and deleted its
  control file, making resume impossible. Values < 1 are rejected at the RPC
  boundary and clamped in `AdjustConns`. (`internal/rpc`, `internal/download`)
- **Negative `--retry` faked success** — a config file with `retry=-1` made
  the per-chunk attempt loop run zero iterations: chunks "succeeded" without
  a single byte downloaded and control files were deleted. Rejected by
  config validation; clamped defensively in `NewTask`.
  (`internal/config`, `internal/download`)
- **Last-completion tally race in the scheduler** — completion reports post
  to the scheduler channel after the WaitGroup release, so when the final
  tasks finished back-to-back, Run could return on its done path with one
  report still buffered — wrong summary line, wrong exit code, missing RPC
  completion event. The done path now drains the buffer first.
  (`internal/scheduler`)

## [1.6.1] - 2026-08-24

### Fixed
Results of a full-project audit (5 critical, 4 major, 5 minor — all fixed
with regression tests):

- **aria2c/both: an exhausted segment no longer silently vanishes** — the
  static queue's requeue was a no-op that reported success, so a segment
  failing through its whole retry budget was skipped and the task reported
  *completed* with an un-downloaded hole (control file deleted, resume
  impossible). The task now fails honestly and `--continue` salvages the
  segments that did land. (`internal/download`)
- **Adaptive 429 back-off actually fires** — the status wrapper never carried
  429 (it was classified retryable), so the halving condition could never
  match. Statuses are now always inspectable, and the restore is cooldown-
  based (30s) instead of the first successful chunk undoing it.
  (`internal/transport`, `internal/ratelimit`)
- **Limiter race on `SetRate("off")`** — a double pointer load let a runtime
  limit change store nil between them, panicking every worker with a nil
  deref. Single load + local check. (`internal/ratelimit`)
- **Metalink4 multi-mirror corruption** — N mirrors spawned N tasks writing
  the same destination concurrently, and the embedded checksum was dropped
  by the batch rule. One task now: primary URL + mirrors, checksum intact.
  (`internal/config`, `cmd/odm`)
- **RPC `changeOption connections` panic** — adjusting a not-yet-started task
  spawned workers on a nil context; refused until Start runs, and Start keeps
  a pre-raised target. Unknown gids now error instead of answering OK.
  (`internal/download`, `internal/rpc`)
- **If-Range is no longer sent to mirrors** (their ETags differ from the
  primary's → false drift detection); ignored-range responses are explicitly
  permanent instead of retried despite the fail-fast contract.
  (`internal/download`)
- **session-log races** — concurrent workers interleaved writes through one
  encoder; serialized under a mutex. Failed chunk attempts roll back their
  progress delta so BytesDone can't exceed TotalSize. (`cmd/odm`,
  `internal/download`)
- **UI display**: aria2c showed `[x16]` while only `--split` workers ran;
  ETA overflowed to 0 for remainders over ~9 GiB; skipped tasks vanished
  from the final frame. All fixed. (`internal/download`, `internal/ui`)
- Smaller: terminal-task pruning sorts numerically; cookie values containing
  tabs stay whole; expired cookies are skipped; probe filename refinement no
  longer races Snapshot readers. (`internal/download`, `internal/config`)

## [1.6.0] - 2026-08-23

### Added
- **`--load-cookies FILE`** — load a Netscape cookies.txt (incl. `#HttpOnly_`
  rows) and send it as a Cookie header; rides the existing `-H` pipeline.
  Cookies never reach the `.odm` control file or logs. (`internal/config`,
  `internal/transport`)
- **`--dry-run`** — probe every URL, show the balancer's plan (mode, per-file
  connections, queue marks, total size) and exit without downloading or
  prompting. (`cmd/odm`)
- **`--auto-rename` / `--skip-existing`** — collision policies for an existing
  destination: save as `name.<N>.ext`, or skip on size match. Gated on the
  absence of a resumable control file, so `--continue` always wins.
  (`internal/download`)
- **`--mirror URL`** (repeatable) — alternate sources for the same file; chunk
  requests rotate round-robin across all sources, per-chunk Content-Range
  validation guards each mirror independently. (`internal/download`)
- **Adaptive slowdown on HTTP 429** — the global limiter halves its rate
  (floor 64 KiB/s) when a server throttles us and restores the configured cap
  after the first successful chunk. Unlimited limiters don't react.
  (`internal/ratelimit`)
- **`--checksum-url URL`** — fetch the digest from a sidecar file
  (sha256sum/md5sum-style, bare hash, or algo:hash) before downloading.
  (`internal/download`)
- **Metalink4 input** — `-i file.meta4` parses mirror URLs + strongest hash;
  first URL is primary, rest become mirrors, verification is automatic.
  (`internal/config`)
- **`--session-log FILE`** — JSONL progress + summary events for wrappers and
  GUIs; append-only, best-effort writes. (`cmd/odm`)

### Changed
- **Engine hardening** — non-retryable 4xx (everything but 408/429) fail a
  chunk after ONE attempt instead of burning the full retry budget × requeue
  passes; resume sends the stored ETag as `If-Range` so server-side drift
  restarts cleanly instead of stitching bytes; retry backoff is exponential
  (capped 30s). (`internal/download`, `internal/transport`)
- **Release artifacts are tar.gz** — `odm_<ver>_<os>_<arch>.tar.gz` containing
  the binary plus LICENSE, replacing raw binaries. PKGBUILD updated to source
  the tarballs. (`.github/scripts`, `packaging/PKGBUILD`)
- Legacy comma-delimited single-argument batch form removed — one positional
  argument is exactly one URL. (`internal/config`)

### Fixed
- Speeds just under 1024 KiB ("1015.3 KiB/s") clipped their `/s` suffix —
  speed column widened to 12 cells. (`internal/ui`)
- Completed task lines keep the fixed grid so bars stay column-aligned with
  active rows; all-done summary right-aligned like live ones; aggregate
  bytes on the Total line. Single-file prompt shows the resolved engine
  under smart. (`internal/ui`)

## [1.5.1] - 2026-08-23

### Fixed
- **Batch summary carries aggregate bytes again** — the Total line shows
  `121.7 MiB/1.8 GiB` next to speed/ETA (and `3.3 MiB/3.3 MiB` on an
  all-done batch) so the whole-batch progress is readable without mental
  math. (`internal/ui/summary.go`)
- **Speeds just under 1024 KiB no longer clip** — "1015.3 KiB/s" is 12 cells
  but the column was 11, truncating the `/s` and jagging the row for a frame;
  the column grew to 12 and the full-layout tier floor moved to 97 columns.
  (`internal/ui/render.go`)
- **Single-file prompt shows the resolved engine under smart** — same
  `[odm — no h2]`-style tag the batch prompt has, as an `Engine:` row.
  (`internal/ui/confirm.go`, `cmd/odm/main.go`)

### Changed
- **Completed task lines keep the full grid** — a finished file's bar lands in
  the same column as the active rows above it (blank speed/ETA cells instead
  of a compact receipt), and its size sits flush against the bar bracket, so
  the batch stays right-aligned from first frame to last. The all-done summary
  is right-aligned the same way. (`internal/ui/render.go`,
  `internal/ui/summary.go`)

## [1.5.0] - 2026-08-23

### Fixed
- **Finished downloads stop showing a phantom ETA "?"** — a completed task's
  ETA cell is blank (nothing remains to estimate) and the batch summary drops
  the `0 B/s  ETA ?` segments once every task is done, leaving only the
  `+<elapsed>` wall-clock total. (`internal/ui/render.go`, `internal/ui/summary.go`)
- **Mid-download range-ignore corruption is fully closed** — a ranged chunk
  request answered `200` (server stops honouring Range mid-flight) is retried
  as a transient error instead of being written at the chunk's offset, and a
  `206` whose `Content-Range` start mismatches the requested offset is now
  rejected the same way. Regression tests cover both (honest-206-then-200s,
  lying-206). (`internal/download/task.go`, `internal/download/rangeignore_test.go`)
- **^C no longer duplicates task lines** — engine logs (`resuming …`,
  retry warnings) are routed through the renderer's frame-safe `Interject`
  printer while a TUI run owns the screen; a bare stderr write between frames
  used to shift the terminal cursor so stale frame rows survived as duplicate
  lines. (`cmd/odm/main.go`, `internal/ui/progress.go`, `internal/logging/logging.go`)
- **Cancelled tasks render as paused, not errors** — after ^C/SIGTERM a task's
  final state is `|` (grey, partial bytes kept) instead of a red error glyph,
  matching the honest cancelled summary. (`internal/download/task.go`)

### Changed
- **Pacman face is eat-synced** — the head shows big `C` exactly when it lands
  on a dot cell and small `c` while travelling between dots (position-driven,
  ILoveCandy-style), replacing the wall-clock c↔C flip every second.
  (`internal/ui/bar.go`)
- **Status glyphs are pure ASCII** (`> ! x + | .`) — Unicode marks like ↓ ✗ ⏸
  are East-Asian-Width ambiguous: some terminals render them two cells wide
  while the width math counts one, so rows silently wrap onto an extra
  physical line at ^C (when every task flips state at once) and the redraw
  contract shatters into duplicated task lines. Frame erases also start from
  a pinned column (`\r`). Pinned by `TestFrameRows_ASCIIOnly`.
  (`internal/ui/summary.go`, `internal/ui/render.go`, `internal/ui/progress.go`)

## [1.4.2] - 2026-08-17

### Added
- **Batch prompt shows the per-file engine for the smart profile** — after the
  Balancer fixes each file's connection budget, the smart decision is resolved
  once per file and shown in the §9 prompt, including *why* (e.g.
  `[odm — no h2]`, `[both — large+wide]`). The resolved engine is injected
  into each task via `SetProfile`, so Start skips the re-resolution (and its
  extra h2 HEAD probe). (`cmd/odm/main.go`, `internal/ui/confirm.go`,
  `internal/download/task.go`, `internal/download/manager.go`)
- **`-sf` is now valid with the smart profile** — smart's engine decision
  consumes the per-file connection budget (`-sf` → Mode C), so rejecting it
  forced smart into 1-connection Mode B where it could never pick `both`.
  `-sf` stays rejected for `aria2c`/`both`. (`internal/config/config.go`)

### Fixed
- **Validation errors dumped the full usage table** — an invalid flag
  combination printed the entire help text, burying the actual error. Now it's
  one line plus a `(run 'odm -h' for usage)` hint. (`cmd/odm/main.go`)
- **Batch prompt "Allocation" could show a remainder-inflated count** — it read
  `Parallel[last].Connections`, which carries a Mode C remainder top-up (5 when
  SF=4) or a single-stream cap (1). It now shows the mode's base (`-sf` in
  Mode C). (`cmd/odm/main.go`)

## [1.4.1] - 2026-08-17

### Fixed
- **`--max-redirect` was dead config** — the value never flowed from
  `config.Options` → `download.ExecOptions` → `transport.NewClient`, so the
  transport client always used Go's zero value (`0`) and refused the *first*
  redirect with "max redirects (0) exceeded". Any URL that redirects once —
  e.g. GitHub release assets via `release-assets.githubusercontent.com` —
  failed the probe and aborted the download. (`cmd/odm/main.go`,
  `internal/download/manager.go`)
- **Mode C queued files got 1 connection instead of `-sf`** — the Connection
  Balancer allocated `SF` connections/file to files running in parallel but
  only `1` to queued files, contradicting PRD §5.4 and the Scheduler's
  documented contract. Queued files now inherit exactly `SF` (still capped to
  1 for single-stream URLs). (`internal/scheduler/balancer.go`)
- **Batch prompt overstated the per-file connection budget** — the §9
  "Allocation" line used the first parallel file's connection count, which
  carries a Mode C remainder top-up, so it displayed e.g. "4 connections/file"
  while queued files actually run with `SF=3`. The prompt now reports the
  honest per-file budget. (`cmd/odm/main.go`)

## [1.4.0] - 2026-08-16

### Added
- **Engine profiles** — `--profile` selects how a file is fetched:
  `odm` (default: fixed work-stealing chunks over HTTP/1.1 multi-connection),
  `aria2c` (static equal split into `--split` segments over HTTP/2 streams —
  the aria2c `-s`/`-x` model, one TCP connection for all segments),
  `both` (50/50 split: the first half via the odm engine, the second via the
  aria2c engine, so both engines run at once), and `smart` (auto-picks the
  engine per file after probing range support, size and h2 readiness). New
  flags: `--split`, `--min-split-size`, `--max-connection-per-server`.
  (`internal/download/engine.go`, `internal/download/smart.go`, `internal/download/task.go`)
- **Modern, fully responsive progress UI** — the pacman bar now leads with a
  per-state status glyph (`↓` downloading, `↻` retrying, `✗` error, `✓` done,
  `⏸` paused, `…` queued), shows an error-count badge (`e<N>`) only when a
  task actually errored, reports `?` instead of a misleading `0%` for sizeless
  streams, and the summary gains an elapsed counter (`+HH:MM:SS`). Layout
  tiers now extend down to a pct-only floor for terminals under 12 columns,
  with every intermediate width (120 → 4 columns) rendering a single
  non-wrapping row. (`internal/ui/render.go`, `internal/ui/progress.go`)

### Fixed
- **HTTP/2 silent-collapse** — `ForceAttemptHTTP2` was `true` and `TLSNextProto`
  was unset, so Go's ALPN negotiation could collapse N worker connections into
  1 HTTP/2 multiplexed connection. The Balancer's N-connection allocation became
  meaningless with no warning. h2 is now unconditionally disabled via
  `ForceAttemptHTTP2: false` + empty `TLSNextProto`. (`internal/transport/transport.go`)
- **`--continue` discarded all progress for the aria2c and both profiles** —
  resume hash verification hashed each completed chunk with the odm `--chunk-size`
  (default 4 MiB) instead of the layout's actual segment size (aria2c static
  splits, usually far larger; the both profile's region2 uses those same
  segments). Hashing only a prefix of the bytes the recorded SHA-256 covers
  never matched, so every interrupted aria2c/both download was discarded as a
  failed integrity check and re-downloaded from scratch. `chunkSpan` now uses
  the layout's real chunk size (`layoutChunkSize`). (`internal/download/task.go`)
- **Resume server-compare sampled only region1 for the both profile** —
  `verifyResumedChunks` sampled `t.queue` (region1) only, so server drift in
  region2 was never detected. The sampler now covers every engine with
  per-region chunk sizes and absolute offsets. (`internal/download/task.go`)
- **Connection reduction could drain ALL workers** — `AdjustConns`' graceful
  drain checked `conns > connTarget` non-atomically, so every worker that read
  the count while it was still above target could retire together, leaving the
  queue with chunks but zero workers — Start then reported the partial file as
  completed and deleted its `.odm`. Retirement is now a CAS (`retireIfAboveTarget`),
  so exactly `live - target` workers exit. (`internal/download/task.go`)
- **Race in `TestRunLoop_WakeTriggersImmediateRedraw`** — the test called
  `Renderer.Frame` directly while the RunLoop goroutine wrote the same
  `bytes.Buffer`; a data race under `-race` (pre-existing). The test now feeds
  snapshots through the loop and reads via a mutex-guarded buffer.
  (`internal/ui/responsive_test.go`)
- **`StaticQueue` offset sort used a hand-rolled insertion sort** — replaced
  with `slices.Sort` (the standard library the rest of the codebase uses).
  No behavior change. (`internal/download/engine.go`)

### Changed
- `progressThrottler` moved from `cmd/odm/main.go` to `internal/rpc/throttler.go`
  (exported as `ProgressThrottler`); no behavior change.
- **`internal/ui` reorganised into thematic files** — `render.go` (task-line
  layout) split into `render.go`, `bar.go` (pacman bar), `ansi.go` (ANSI-aware
  string primitives), `summary.go` (humanisers + aggregate summary + status
  glyphs), and `color.go` (colour constants + state map). Pure logic now lives
  in one place each; no behavior change. (`internal/ui/*`)

### Fixed
- **^C rendered doubled/interleaved task lines** — two goroutines wrote the
  final frame: `RunLoop`'s `ctx.Done` case and `main`'s post-run `Frame(nil,nil)`.
  `RunLoop` no longer renders a final frame on cancel (it only restores the
  cursor and closes a `done` channel); `main` waits for the loop to fully exit
  (`<-uiDone`) before issuing the single final frame. No more doubled,
  `0%`-fused lines on Ctrl-C. (`internal/ui/progress.go`, `cmd/odm/main.go`)
- **^C reported `0 failed` in the summary** — `Scheduler.Run` returned on
  `ctx.Done` without draining `s.compl`, so in-flight tasks cancelled by ^C were
  never tallied. Run now drains completion reports until no task is live, then
  returns with the correct succeeded/failed counts. (`internal/scheduler/queue.go`)
- **Daemon shutdown deadlock** — after the tally fix, `Run`'s select could busy-
  loop on a closed `doneCh` while in-flight tasks still posted to `s.compl`,
  hanging `Daemon.Stop`. The post-cancel path now drains `s.compl` directly.
  (`internal/scheduler/queue.go`)
- **WaitGroup Add-vs-Wait race on daemon shutdown** — `Enqueue`/`admitNext`
  called `wg.Add` from the RPC goroutine while `Run`'s background `wg.Wait`
  observed the count; once the count hit zero, a racing Add was undefined
  behavior. Every Add now goes through `AddCounter`, which refuses once
  `windingDown` is set. (`internal/scheduler/queue.go`)
- **Pacman face animated off the render frame counter** — `c`/`C` toggled on
  every frame, so a burst of frames (SIGWINCH resize storm, event flush) made
  the face flicker gede-kecil rapidly. The face is now wall-clock-driven
  (`faceTick` in seconds, toggling every `pacFaceInterval`), independent of
  render cadence, and the aggregate summary bar animates in step with the
  per-file bars. (`internal/ui/bar.go`, `internal/ui/progress.go`)
- **Race on task layout between RPC `AdjustConns` and `Start`** — the daemon
  admits a task the moment `NewTask` returns, so `changeOption → connections`
  could read `t.engines`/`t.queue` while `Start` was building the layout.
  Layout writes are now guarded by `layoutMu`, read under the same lock.
  (`internal/download/task.go`)
- **`both` profile oversubscribed the connection budget at `-c 1`** —
  `max(1,conns/2) + max(1,conns-conns1)` spawned 2 workers on a 1-connection
  budget, doubling TCP connections. `conns < 2` now degrades to the
  single-region odm engine. (`internal/download/task.go`)
- **RPC daemon could corrupt output via config `output`** — `-o` is single-file
  only, but the daemon path never cleared `exec.OutFile`, so every `addUri`
  task wrote the same path (overlapping `WriteAt` = silent corruption). The
  daemon now clears it. (`cmd/odm/main.go`)
- **`restoreChunkHashes` dropped region2 hashes on resume** — control-file
  hashes are keyed by absolute offset, but the resume restore looked up
  region2's relative offsets, silently dropping them and downgrading the next
  resume to the server-compare fallback. Offsets are now translated to absolute
  per region. (`internal/download/task.go`)
- **Resume sampling used the wrong client for region2** — `verifyResumedChunks`
  sampled every span via the h1 client even for spans downloaded over h2; a
  valid resume could be flagged as a mismatch. Spans are now sampled through
  their owning region's client. (`internal/download/task.go`)
- **Probe goroutines leaked on ^C** — up to 8 workers could block on `probeCh`
  while the consumer had already returned. Workers and the consumer now honour
  `ctx.Done()`. (`cmd/odm/main.go`)
- **`signalCtx` goroutine leaked** — the signal-handler goroutine parked on the
  channel forever; it now exits via `ctx.Done()` and the handler is stopped on
  cancel. (`cmd/odm/main.go`)
- **`smart` profile was inert for batch downloads** — it used the global `-c`
  instead of the per-file allocation the Balancer set, so batch files (1 conn)
  never reached the h2/both thresholds. It now reads the per-file budget.
  (`internal/download/task.go`)
- **`--profile smart` with `-c 1` on `both`** — same oversubscribe guard as the
  explicit `both` profile. (`internal/download/task.go`)

### Changed
- Dead fields removed: `Task.startAt`, `Renderer.lastLoggedPct`; `colorReplace`
  now uses `strings.Replace`; `emit()` reuses `LiveViews`/`QueuedViews`;
  `StaticQueue.ResetCompletedOffsets` documents its unused `totalSize` param.
  (`internal/download/task.go`, `internal/ui/*`)
- Batch confirmation prompt now aligns the file list into a table (names padded
  to 40 cells, sizes right-aligned to 14) in both colour and plain modes.
  (`internal/ui/confirm.go`)
- Summary line: speed is yellow and elapsed cyan on a TTY; paused/queued
  task percentages are grey, completing the per-state colour story.
  (`internal/ui/summary.go`, `internal/ui/render.go`)

## [1.3.1] - 2026-08-01

### Removed
- Dead-code sweep (~570 lines): unused accessors, wrappers and helpers across
  the internal packages — `Manager.Opts`/`VerifyChecksum`/`ErrNoTasks`,
  `Task.Filename`/`SupportsRange`/`Connections`/`getCurrent`,
  `ChunkQueue.Remaining`/`CompletedCount`/`validateChunks`,
  `Scheduler.SortPlan`/`emitThrottled`/`LiveCount`/`QueuedCount`,
  `rpc.BroadcastProgress`/`NewServer`/`codeInvalidRequest`/`parseIntParam`,
  `storage.Size`/`Path`/`Reader` + control-file helpers,
  `config.Default`, `Limiter.BytesPerSec`, `ui.RenderTaskLine`/`truncateName`/
  `ansiVisibleWidth`, and the `config.Version`/`download.Version` aliases.

### Changed
- Content-Disposition parsing now uses stdlib `mime.ParseMediaType`;
  `GetRange` returns `*http.Response` directly (dead fields dropped).
- `ui` sorting uses `slices.SortStableFunc`; `ParseRate` accepts
  lowercase-`b` suffixes (e.g. `5Kb`).
- `Logger.file` is typed `*os.File`, fixing a latent panic if a non-file
  writer were ever substituted.

## [1.3.0] - 2026-08-01

### Added
- **True SOCKS5 / SOCKS5h proxy support** — `--proxy socks5://…` and
  `socks5h://…` now speak the SOCKS5 protocol (RFC 1928) instead of falling
  back to HTTP CONNECT tunnelling. `socks5` resolves hostnames locally and
  sends the IP to the proxy; `socks5h` sends the hostname for proxy-side DNS.
  (`internal/transport`, `golang.org/x/net/proxy`)
- **Per-chunk resume verification** — the `.odm` control file now records a
  SHA-256 hash per completed chunk; resuming verifies every completed chunk
  from local disk, so silent disk corruption is caught instead of being
  resumed into a corrupt file. Legacy control files (no hashes) keep the
  server-side spot-check. (`internal/storage/resume.go`, `internal/download/task.go`)
- **Chunk-boundary invariant check** — `NewChunkQueue` validates that chunks
  are contiguous, non-overlapping, and cover the whole file; a boundary
  programming error now fails loudly instead of corrupting data silently.
  (`internal/download/chunkqueue.go`)

### Changed
- **`--checksum` in batch mode now warns** — when multiple URLs are given the
  flag is ignored (one hash cannot cover many files); the CLI now prints a
  warning to stderr instead of dropping it silently. (`cmd/odm/main.go`)
- **Version is single-sourced** — `config.Version` and `download.Version` are
  compile-time aliases of `odm/internal/version`; a mismatch is now a build
  error, and the release checklist is down to two locations (`version.go` +
  `PKGBUILD`). (`internal/version/version.go`)
- **`WriteAt` invariant documented** — concurrent writers must use disjoint
  byte ranges; the engine guarantees this via chunk boundaries. No behavior
  change, but the contract is now explicit. (`internal/storage/file.go`)

### Fixed
- **Pause could leave workers stuck forever** — `Unpause` sent once on a
  buffered channel, waking only ONE of N blocked workers; the rest stayed in
  the pause gate and `Wait()` hung. Unpause now broadcasts to all workers.
  Regression-tested with 4 workers. (`internal/download/task.go`,
  `internal/download/pause_test.go`)
- **Resume could silently build a mixed-version file** — with per-chunk hashes
  present, the server-side drift check was skipped entirely; a same-size,
  no-ETag server-side content change during an interrupted download produced a
  file part-old part-new, reported as success. Both guarantees now run:
  local hash verification AND the sampled server compare. (`internal/download/task.go`)
- **Partial hash coverage no longer discards progress** — a control file
  written by an older version (no hashes), resumed once, then interrupted
  again, has hashes only for the new chunks; resuming now falls back to the
  server compare instead of erroring and re-downloading from scratch.
  (`internal/download/task.go`)
- **Resume span computation defaulted chunk size inconsistently** — a
  non-positive `ChunkSize` in `ExecOptions` would make resume-hash spans
  invalid and always trigger a full re-download; the 4 MiB default is now
  shared between queue layout and resume verification. (`internal/download/chunkqueue.go`)

## [1.2.0] - 2026-07-31

> **Chunk re-queue, resume integrity checks, data-corruption fixes, and a batch
> of reliability fixes across the engine, scheduler, RPC and UI.**
>
> The headline is correctness: single-stream downloads can no longer silently
> corrupt files (or report success on them), ^C now exits with code 4
> (cancelled), `--checksum` verifies the actual written file, and a failed chunk
> is re-queued instead of aborting the whole task. Resume spot-checks on-disk
> data against the server, the `.odm` control file is flushed on a bounded
> interval instead of per-chunk, RPC `remove` works on queued tasks, slot
> admission is atomic, and 0-byte tasks finally render in the progress list.

### Fixed
- **Data corruption on single-stream downloads with a known size** — when a
  server reported `Content-Length` but ignored `Range` (always serving 200 with
  the full body), the engine split the file into multiple chunks, special-cased
  only chunk 0 as a plain GET, then wrote the *full* body of subsequent chunks
  at their offset. The result was an oversized, interleaved file that the task
  nonetheless reported as **completed** (bytesDone over-counted past TotalSize
  and the error check compared the wrong way), and the `.odm` resume file was
  deleted. Single-stream downloads now always use exactly one whole-file chunk,
  and completion is decided by chunk errors rather than a byte-count comparison.
  (`internal/download/task.go`)
- **`--checksum` could verify the wrong file** — verification ran against a
  URL-derived path (`ResolveDest`) instead of the actual written file, so a
  server-provided `Content-Disposition` filename made the check read a
  non-existent (or stale) file. The engine now verifies the real output path
  per-task; with multiple URLs the flag is ignored as before. (`internal/download/task.go`, `cmd/odm/main.go`)
- **Wrong exit code on ^C / SIGTERM** — cancelling a download returned exit
  code 1 (general) or 2/3 (network/partial) instead of the documented 4
  (cancelled), because in-flight tasks were counted as failures. A user-initiated
  cancel now maps to `ExitCancelled`. (`cmd/odm/main.go`)
- **`--output/-o` with multiple URLs silently overwrote one file** — every task
  resolved to the same destination. Now rejected with a clear error.
  (`cmd/odm/main.go`)
- **Data race in the rate limiter** — `SetRate` (RPC `changeOption`) wrote the
  limiter fields while workers read them concurrently. The limiter and per-task
  limiter are now held atomically. (`internal/ratelimit/bucket.go`,
  `internal/download/task.go`)
- **Flaky RPC test** — `TestServer_AddURIAndTellActive` could miss a task whose
  probe fails faster than the poll loop; it now also polls `tellStopped`.
  (`internal/rpc/server_test.go`)
- **A failed chunk no longer aborts the whole download** — a worker that
  exhausts its per-chunk retry budget now returns the chunk to the queue (bounded
  by `--retry` worker-level passes) instead of failing the task, so a transient
  error on one range doesn't discard the rest of the file. (`internal/download/chunkqueue.go`, `internal/download/task.go`)
- **Resume validates on-disk data against the server** — when `--continue` finds
  completed chunks, a sample of them is spot-checked with ranged GETs; a
  silently-changed file (no ETag) or corrupt local bytes triggers a full
  re-download instead of resuming into a corrupt result. A control file whose
  layout no longer matches the URL (e.g. ranged resume hitting a single-stream
  server) is likewise discarded. (`internal/download/task.go`)
- **Control-file writes are bounded, not per-chunk** — the `.odm` file used to
  be rewritten on every completed chunk, which is O(n²) on large files (the
  completed-offset list grows with each chunk). It now flushes at most every 16
  chunks or 2 seconds, and concurrent worker writes are serialized so the file
  can't be corrupted by interleaved temp-writes. (`internal/download/task.go`)
- **RPC `remove` now works on queued tasks** — a task cancelled before it
  started used to be a silent no-op (`Cancel` had no context to cancel), so it
  would still download once a slot freed. Cancel now flags the task and `Start`
  fails fast without touching the server. (`internal/download/task.go`)
- **Scheduler slot admission is atomic** — the free-slot check, queue pop and
  live-map insert now happen in one critical section, so a concurrent RPC
  `Enqueue` and the Run loop can no longer both pass the check and over-subscribe
  slots by one. The completion channel is also created in the constructor,
  fixing a latent daemon race where `Enqueue` could read it while `Run` assigned
  it. (`internal/scheduler/queue.go`)
- **WebSocket subscribers no longer leak on write failure** — a broken client
  whose write failed but whose read side stayed open left the read-loop
  goroutine running until the socket died; both loops now signal each other to
  exit. (`internal/rpc/ws.go`)
- **`odm.shutdown` replies before stopping** — the daemon stop used to run
  concurrently with the response write, risking a truncated "OK"; the response
  is now flushed first, then the daemon winds down. (`internal/rpc/server.go`)
- **0-byte completed/error tasks render in the per-file list** — an empty file
  (or a failed probe) vanished from the list while still counting in the
  summary; terminal states now always display. (`internal/ui/progress.go`)

### Changed
- **No more double probe** — the CLI already probed every URL for the Balancer
  and confirmation prompt; that result is now reused by each task (`SetProbe`),
  so a download fires one probe instead of two. The RPC daemon path still
  probes at task start as before. (`internal/download/task.go`, `cmd/odm/main.go`)
- **Bounded task retention** — the RPC daemon's task registry and the
  `tellStopped` list no longer grow forever: terminal (completed/error) tasks
  are pruned oldest-first past a cap, so a long-lived daemon's memory stays
  flat. (`internal/download/manager.go`, `internal/scheduler/queue.go`)
- **Progress bar shrinks on narrow terminals** — the fixed 30-cell bar plus the
  info block needed ~96 columns; below that the bar (and only the bar) now
  gives way so the percent column stays on screen instead of being truncated.
  (`internal/ui/render.go`, `internal/ui/progress.go`)
- **Wide CJK/emoji filenames are measured in display cells, not runes** — a
  CJK name counts 2 cells per character, so it truncates and pads correctly
  instead of pushing the info block past the terminal edge. (`internal/ui/render.go`)

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

[Unreleased]: https://github.com/Fahry-a/ODM/compare/v1.7.2...HEAD
[1.7.2]: https://github.com/Fahry-a/ODM/compare/v1.7.1...v1.7.2
[1.7.1]: https://github.com/Fahry-a/ODM/compare/v1.7.0...v1.7.1
[1.7.0]: https://github.com/Fahry-a/ODM/compare/v1.6.1...v1.7.0
[1.6.1]: https://github.com/Fahry-a/ODM/compare/v1.6.0...v1.6.1
[1.6.0]: https://github.com/Fahry-a/ODM/compare/v1.5.1...v1.6.0
[1.5.1]: https://github.com/Fahry-a/ODM/compare/v1.5.0...v1.5.1
[1.5.0]: https://github.com/Fahry-a/ODM/compare/v1.4.2...v1.5.0
[1.4.2]: https://github.com/Fahry-a/ODM/compare/v1.4.1...v1.4.2
[1.4.1]: https://github.com/Fahry-a/ODM/compare/v1.4.0...v1.4.1
[1.4.0]: https://github.com/Fahry-a/ODM/compare/v1.3.1...v1.4.0
[1.3.1]: https://github.com/Fahry-a/ODM/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/Fahry-a/ODM/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/Fahry-a/ODM/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/Fahry-a/ODM/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/Fahry-a/ODM/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/Fahry-a/ODM/compare/v0.3.0...v1.0.0
[0.3.0]: https://github.com/Fahry-a/ODM/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Fahry-a/ODM/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Fahry-a/ODM/releases/tag/v0.1.0
