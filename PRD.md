# PRD — ODM (Oryn Download Manager)

| | |
|---|---|
| **Product Name** | ODM — Oryn Download Manager |
| **Binary Name** | `odm` |
| **Language** | Go (Golang) |
| **Inspired By** | aria2c |
| **License** | MIT |
| **Document Version** | 1.4 |
| **Date** | July 20, 2026 |
| **Status** | Draft — ready for implementation (vibe coding via Claude Code) |

---

## 1. Product Overview

ODM is a CLI download manager written in Go, inspired by `aria2c`. Its core differentiator is the **Connection Balancer** — a system that automatically allocates parallel connections depending on whether the user is downloading a single file or many files at once (batch), without the user having to manually compute connections per file.

ODM also ships with a pacman/CachyOS-style (`ILoveCandy`) progress bar in the terminal, and an RPC server (JSON-RPC 2.0 + WebSocket) so other programs can build wrappers/GUIs on top of ODM, similar to the `aria2c` + `AriaNg` ecosystem.

## 2. Background & Goals

### 2.1 Problem Statement
- `aria2c` is very powerful, but the allocation between "many files" vs "many connections per file" has to be set manually and doesn't automatically adapt to one another.
- There is no modern Go-based CLI download manager (single binary, no runtime dependencies) with a pacman-style progress bar familiar to Arch/CachyOS users.

### 2.2 Goals
- Single static Go binary, easy to cross-compile, no external runtime dependency.
- Support multi-connection download per file (default cap 32 connections, configurable higher with an explicit warning — see §5.1).
- Support batch downloads (multiple files, multiple URLs).
- **Connection Balancer**: automatic and consistent connection allocation between single-file mode and batch mode.
- pacman/CachyOS-style (`ILoveCandy`) progress bar.
- RPC interface so the tool can be wrapped by other applications (other CLIs, GUIs, scripts).
- Configuration via file (`/etc/odm/config.conf`) and via CLI arguments (CLI overrides config).

### 2.3 Non-Goals (out of scope for the initial version)
- BitTorrent / magnet links (could be added to the roadmap, not MVP).
- An official GUI (RPC is provided so third parties can build their own GUI).
- Automatic distributed/multi-mirror server selection (could be a future feature).

## 3. Terminology

| Term | Meaning |
|---|---|
| **Task** | One unit of download work for a single URL/file. |
| **Segment** | One byte-range chunk of a file downloaded over a single HTTP connection. |
| **Connection** | One active HTTP connection downloading one segment. |
| **Connection Budget (`-c`)** | Total parallel-connection allowance calculated by the Balancer. |
| **Split-File (`-sf`)** | Number of parallel connections per file during batch downloads (optional). |
| **Balancer** | The module that computes how many files run in parallel and how many connections each gets. |
| **Batch** | Mode for downloading many URLs at once in a single command. |

## 4. Example Behavior (from the original spec)

```bash
# 1. Single file, 16 parallel connections to one server for one file
odm -c 16 https://files.test.xyz/file.tar.gz

# 2. Batch, without -sf: 16 URLs, 1 connection per file, 16 files run in parallel
odm -c 16 "https://files.test.xyz/file1.tar.gz,url2,url3,...(16 urls)"

# 3. Batch, with -sf 4: 16 connections split 4 per file -> 4 files run in parallel
odm -c 16 -sf 4 "https://files.test.xyz/file1.tar.gz,url2,url3,...(16 urls)"
```

> The comma-joined string above is the syntax from the original spec. See §6.4 for the recommended (and safer) input method.

## 5. Connection Balancer Specification (Core Feature)

This is ODM's main differentiating feature, so it must be implemented precisely.

### 5.1 Balancer Inputs
- `C` = total connection budget (from `-c`).
- `N` = number of URLs provided (1 for single file, >1 for batch).
- `SF` = split-file per file (from `-sf`, optional, only relevant when `N > 1`).
- `MaxConnections` = configurable ceiling (from `--max-connections`, see §6.2), **default 32**.

**Why cap at 32 by default:** many servers/CDNs treat >30-ish simultaneous connections from one IP as abusive and start throttling or blocking the client; returns also diminish once available bandwidth is saturated. 32 is kept as the *default*, not a hardcoded limit — power users on high-bandwidth links (LAN, local CDN) can raise it via `--max-connections`. If `C > MaxConnections`, ODM prints a warning (`"connections above 32 may get throttled/blocked by some servers"`) but still proceeds, since the user explicitly opted in.

### 5.2 Mode A — Single File (`N == 1`)
The entire connection budget is allocated to that one file.

```
connections_for_file = min(C, MaxConnections)
```

**Range-support probe (3-step fallback chain)**, since many servers block `HEAD`, misreport `Content-Length`, or omit `Accept-Ranges` even though ranged `GET` actually works:

```
1. Try HEAD request
     -> got Content-Length + Accept-Ranges: bytes?  use it.
2. HEAD failed/incomplete? Try GET with "Range: bytes=0-0"
     -> server replies 206 Partial Content?  ranged requests supported, use Content-Range for total size.
3. Still no usable size/range support?  connections_for_file = 1 (single-stream fallback, plain GET).
```

### 5.3 Mode B — Batch without `-sf` (`N > 1`, `SF` not set)
Each file gets **1 connection**. What `C` controls here is how many files run **in parallel** at any given time (similar to `--max-concurrent-downloads` in aria2).

```
parallel_files = min(C, N, MaxConnections)
```

If `N > C`, the remaining files go into a **queue** and automatically start as slots free up (other files finish/fail).

### 5.4 Mode C — Batch with `-sf` (`N > 1`, `SF` set)
Each file gets `SF` connections, and the number of parallel files is derived automatically from the remaining budget:

```
parallel_files    = max(1, floor(C / SF))
parallel_files    = min(parallel_files, N)
connections_used  = parallel_files * SF
remainder         = C - connections_used
```

The `remainder` (leftover connections that don't divide evenly) is distributed one at a time to the first currently-running parallel files, so the `C` budget is used to the fullest without exceeding the cap:

```
for i in 0..remainder-1:
    connections[i] += 1
```

**Example:** `-c 16 -sf 5` with 10 URLs:
```
parallel_files = floor(16/5) = 3
connections_used = 15, remainder = 1
=> file#1: 6 connections, file#2: 5 connections, file#3: 5 connections (total 16)
remaining 7 URLs go into the queue
```

Queued URLs still use the same `SF`-based allocation scheme once they get a slot.

### 5.5 Validation & Edge Cases
- `C >= 1`; `C > MaxConnections` is allowed but prints a warning (see §5.1). `C < 1` → clear error, program does not run.
- If `SF > C` → error: `"split-file (-sf) cannot be greater than the total connection budget (-c)"`.
- If `SF` is set but `N == 1` → `-sf` is ignored with a warning (Mode A already claims the entire budget).
- **Allocation-time reallocation (MVP):** the range-support probe (§5.2) runs for every URL *before* the Balancer computes the final split. If a file turns out not to support ranged requests, its connections are capped to 1 and the freed budget is redistributed to the other files being allocated at that same scheduling pass (extra connections added the same way as the `remainder` distribution in §5.4).
- **Mid-flight dynamic reallocation** (rebalancing connections of segments that are already downloading) is **not** in MVP — it requires live goroutine renegotiation and is tracked as a future improvement in §15.

## 6. CLI Specification

### 6.1 Command Format
```
odm [OPTIONS] <URL | "URL1,URL2,URL3,...">
odm [OPTIONS] -i <file-list.txt>
odm --rpc [OPTIONS]
```

### 6.2 Argument List

| Flag | Alias | Description | Default |
|---|---|---|---|
| `--connections` | `-c` | Total connection budget | `5` |
| `--max-connections` | | Configurable ceiling for `-c`/`-sf`; going above it just prints a warning, not an error | `32` |
| `--split-file` | `-sf` | Parallel connections per file during batch downloads | *(unset = Mode B)* |
| `--output` | `-o` | Output file name (single-file mode only) | derived from URL |
| `--dir` | `-d` | Destination directory | from config / cwd |
| `--input-file` | `-i` | Read URL list from a file (one URL per line) | — |
| `--yes` | `-y` | Skip the confirmation prompt | `false` |
| `--quiet` | `-q` | Disable the progress bar (for cron/scripts) | `false` |
| `--continue` | `-x` | Resume an incomplete file (uses the `.odm` control file) | `true` |
| `--max-redirect` | | Max number of redirect hops to follow | `5` |
| `--retry` | | Number of retries per segment on failure | `3` |
| `--retry-wait` | | Delay between retries (seconds) | `2` |
| `--timeout` | | Connection timeout (seconds) | `30` |
| `--user-agent` | | Custom User-Agent header | `odm/<version>` |
| `--header` | `-H` | Add a custom HTTP header (repeatable) | — |
| `--referer` | | Set the Referer header | — |
| `--proxy` | | Proxy (http/https/socks5) | — |
| `--check-certificate` | | Verify TLS certificates | `true` |
| `--checksum` | | Verify checksum, format `algo:hash` (md5/sha1/sha256) | — |
| `--limit-rate` | | Global speed limit, e.g. `5M`, `500K` | unlimited |
| `--config` | | Path to a custom config file | `/etc/odm/config.conf` |
| `--log` | | Log file path | — |
| `--log-level` | | `debug` / `info` / `warn` / `error` | `info` |
| `--rpc` | | Run as an RPC server (daemon mode) | `false` |
| `--rpc-listen-port` | | RPC server port | `6900` |
| `--rpc-listen-all` | | Bind to `0.0.0.0` (default `127.0.0.1`) | `false` |
| `--rpc-secret` | | RPC authentication token | — |
| `--version` | `-V` | Show version | |
| `--help` | `-h` | Show help | |

### 6.3 Configuration Source Priority
```
CLI args  >  ~/.config/odm/config.conf  >  /etc/odm/config.conf  >  hardcoded defaults
```

### 6.4 Batch URL Input — Delimiter Design Decision

The original spec proposed joining all batch URLs into a single comma-separated string (`odm -c 16 "url1","url2","url3"`, which the shell collapses into one argument: `url1,url2,url3`). This breaks if any URL contains a literal comma — which is legal in a query string, e.g. `?ids=1,2,3` — since ODM can't distinguish a URL-internal comma from a list separator.

**Decision: support both, but make space-separated positional arguments the primary/recommended form.**

```bash
# Recommended — space-separated positional args; the shell tokenizes them natively,
# so a comma anywhere inside a URL is never ambiguous.
odm -c 16 url1 url2 url3 ...

# Still supported for convenience / backward-compat with the original syntax.
odm -c 16 "url1,url2,url3,..."
```

- **Parsing rule:** if multiple positional arguments are passed, each one is treated as a single URL (canonical form). If exactly one positional argument is passed and it contains commas, it's treated as a legacy comma-delimited list.
- `--help` and the docs should warn that URLs containing a literal comma must use the space-separated form or `-i <file>`.
- For large batches (roughly >10 URLs), `-i <file>` is recommended regardless of delimiter — easier to version/maintain and avoids shell argument-length limits.

## 7. Configuration File Format (`/etc/odm/config.conf`)

`key = value` format, one option per line, `#` for comments, key names match the CLI long-flags (without `--`).

```ini
# /etc/odm/config.conf
# Global default configuration for ODM

connections        = 5
max-connections    = 32
split-file         =
dir                 = /home/user/Downloads
max-redirect        = 5
retry                = 3
retry-wait           = 2
timeout              = 30
user-agent           = odm/1.0
check-certificate    = true
continue             = true
quiet                = false
limit-rate           =

# RPC
rpc                  = false
rpc-listen-port      = 6900
rpc-listen-all       = false
rpc-secret           =

# Logging
log                  =
log-level            = info
```

## 8. Progress Bar — Pacman / CachyOS Style (`ILoveCandy`)

Reference: the `ILoveCandy` option in `pacman.conf` replaces the plain `#####----` progress bar with a pacman-eating-dots animation, commonly seen on Arch/CachyOS during a system update.

**How the animation works:** the portion already downloaded is "eaten" and rendered as blank/dashes; the pacman icon (`c`) marks the current position; the portion not yet downloaded is shown as dots (`o`). At 100%, the entire bar has been eaten — it renders as blank/dashes all the way through, since there's nothing left for pacman to eat.

### 8.1 Per-File Line Format
```
<file_name>   <size>   <speed>/s   <ETA>   [x<N>]   [<progress-bar>]   <percent>%
```

Example (matching the original spec, now with the `[x<N>]` connection-count indicator and the corrected 100% bar):
```
linux-cachyos    120.5 MiB  25.4 MiB/s  00:05  [x16]  [-------------]  100%
firefox          85.2 MiB   18.9 MiB/s  00:04  [x4]   [---c o o o o]   72%
```

- `[x<N>]` = number of parallel connections currently used by that file (the Balancer's output).
- `c` = the pacman icon, moving left to right as progress advances; `o` = a dot not yet "eaten"; blank/`-` = already eaten. At 100% the whole bar is blank/dashes (fully eaten), as shown on the `linux-cachyos` line above.
- During batch downloads, each file gets its own line that's redrawn in place (via ANSI cursor control), plus a summary line at the bottom:

**Color coding** (when the terminal supports ANSI colors; disabled automatically on non-TTY or `NO_COLOR`):
| State | Color |
|---|---|
| Completed (100%) | green |
| Downloading | yellow |
| Retrying after an error | red |
| Queued / waiting | dim/grey |

```
Total: 3/16 completed  |  44.3 MiB/s  |  ETA 00:32
```

### 8.2 Non-TTY Fallback
If stdout is not a terminal (redirected to a file/pipe) or `--quiet` is set, the progress bar is replaced with periodic log lines (e.g. every 10% or every fixed time interval), without ANSI cursor control.

## 9. Confirmation Prompt

Shown before a download starts, unless `-y`/`--yes` or `--quiet` is set. Always skipped in `--rpc` (daemon) mode, since there is no interactive terminal to confirm against.

**Single-file mode:**
```
File       : linux-cachyos-6.10.tar.zst
Size       : 120.5 MiB
Connections: 16 parallel
Destination: /home/user/Downloads/linux-cachyos-6.10.tar.zst

Continue? [Y/n]
```

**Batch mode:**
```
ODM will download 16 files (total ~1.2 GiB)
Allocation: 4 connections/file, 4 files running in parallel (rest queued automatically)

  [1] linux-cachyos-6.10.tar.zst    120.5 MiB
  [2] firefox-140.0.tar.xz           85.2 MiB
  ...

Continue? [Y/n]
```

## 10. RPC Interface

Goal: let other programs (other CLIs, GUIs, automation scripts) build wrappers on top of ODM without parsing terminal output — the same relationship `aria2c` has with `AriaNg`.

### 10.1 Transport
- **JSON-RPC 2.0 over HTTP POST**: `http://127.0.0.1:6900/rpc`
- **WebSocket** for real-time event notifications: `ws://127.0.0.1:6900/ws`
- Default bind is `127.0.0.1` (safe); `--rpc-listen-all` binds to `0.0.0.0`.
- Authentication via `--rpc-secret`, sent as a `"token:<secret>"` parameter on every request (same pattern as aria2).

### 10.2 Method List

| Method | Description |
|---|---|
| `odm.addUri` | Add a single new URL to the download queue. |
| `odm.addBatch` | Add many URLs at once (with `-sf` options). |
| `odm.pause` / `odm.pauseAll` | Pause a specific task / all tasks. |
| `odm.unpause` / `odm.unpauseAll` | Resume paused task(s). |
| `odm.remove` | Cancel and remove a task from the queue/active list. |
| `odm.tellStatus` | Detailed status of one task (progress, speed, connection count, ETA). |
| `odm.tellActive` | List of currently active tasks. |
| `odm.tellWaiting` | List of queued tasks. |
| `odm.tellStopped` | List of completed/failed/cancelled tasks. |
| `odm.changeOption` | Change an option on a running task (e.g. `limit-rate`). |
| `odm.getGlobalStat` | Global statistics (total speed, number of active tasks). |
| `odm.getVersion` | Version info & supported features. |
| `odm.shutdown` | Shut down the ODM RPC daemon. |

### 10.3 Event Notifications (via WebSocket)
`onDownloadStart`, `onDownloadProgress`, `onDownloadComplete`, `onDownloadError`, `onDownloadPause`.

## 11. Architecture & Go Project Layout

```
odm/
├── cmd/
│   └── odm/
│       └── main.go            # entry point, CLI parsing (cobra/pflag)
├── internal/
│   ├── scheduler/
│   │   ├── balancer.go        # implementation of the algorithm in §5
│   │   └── queue.go           # batch download queueing & parallel-slot scheduling
│   ├── download/
│   │   ├── manager.go         # orchestrates tasks, owns the progress aggregator
│   │   ├── task.go            # represents one file download
│   │   └── chunkqueue.go      # work-stealing chunk queue per task (§11.1)
│   ├── storage/
│   │   ├── file.go            # pre-allocation, WriteAt wrapper
│   │   └── resume.go          # .odm control file read/write (§11.3)
│   ├── transport/
│   │   └── http.go            # http.Client wrapper: redirects, retry, headers, proxy, range probe
│   ├── ratelimit/
│   │   └── bucket.go          # global token-bucket limiter (§11.4)
│   ├── config/
│   │   └── config.go          # config file parser + merge with CLI flags
│   ├── ui/
│   │   └── progress.go        # pacman-style progress bar renderer, color states
│   └── rpc/
│       ├── server.go          # JSON-RPC 2.0 HTTP handler
│       └── ws.go              # WebSocket event broadcaster
├── configs/
│   └── odm.conf.example
├── go.mod
├── LICENSE                     # MIT
└── README.md
```

### 11.1 Concurrency Model — Chunk Queue (Work-Stealing)

Instead of statically pre-assigning one fixed byte-range per connection, each task splits its file into many small chunks (e.g. 2–4 MiB) and pushes them into a shared, per-task chunk queue. Worker goroutines (one per allocated connection) pull the next available chunk as soon as they finish the previous one.

This avoids the classic **straggler problem** of static equal-split segmentation: with a fixed split, one slow/congested connection holds back the whole file even though every other connection finished early. With a chunk queue, a slow worker just processes fewer chunks overall — the fast workers pick up the slack automatically.

```
chunk queue: [chunk0][chunk1][chunk2]...[chunkN]
worker 1 ──┐
worker 2 ──┼─► pull next chunk → download → write via storage.WriteAt(offset) → pull next
worker N ──┘
```

- Chunk size is configurable (`--chunk-size`, default e.g. `4M`) — small files use fewer/smaller chunks so they don't end up with a queue longer than useful.
- Each worker goroutine is coordinated via `context.Context` for cancel/pause.
- Each chunk write goes to the destination file via `file.WriteAt(buf, offset)` — chunk boundaries never overlap, so no file lock is needed for concurrent writes.
- Per-chunk progress is sent to a central aggregator over a channel, throttled (e.g. every 100ms) before being rendered to the UI/RPC layer to avoid flooding.
- The destination file is pre-allocated (`os.Truncate` to `Content-Length`) before workers start writing.

### 11.2 Concurrency Model — Single-Stream Fallback
When a file doesn't support ranged requests (§5.2, fallback chain step 3), the chunk queue degenerates to a single chunk covering the whole file, downloaded by one worker via a plain sequential `GET`.

### 11.3 Resume Support
Every task has a `<filename>.odm` control file (similar to aria2c's `.aria2`), containing JSON: URL, total size, chunk size, and the list of chunks already completed. When `--continue` is enabled and a control file is found, ODM re-queues only the chunks not yet marked done instead of starting over.

### 11.4 Rate Limiting
`--limit-rate` is enforced with a **global token bucket** shared across all active workers/tasks, rather than splitting the limit evenly per connection. This is simpler to reason about and produces a more stable aggregate speed than dividing `limit-rate / connection-count`, especially when the number of active connections changes over time (files finishing, batch queue advancing).

The global token bucket operates on bytes read from the network (`io.Reader`) before writing them to disk. Every active worker acquires tokens proportional to the number of bytes it is about to read, ensuring the aggregate download throughput never exceeds `--limit-rate` regardless of the number of workers, tasks, or connection reallocation. This means throttling happens at the data-stream level, not at the request or connection level — a worker with a slow request still consumes tokens strictly in proportion to bytes actually transferred.

## 12. Download Workflow (High-Level Flow)

1. Parse CLI args → merge with the config file (§6.3).
2. For each URL: run the 3-step range-support probe (§5.2 — HEAD → ranged GET probe → single-stream fallback) to determine `Content-Length`/range support, and resolve the redirect chain (up to `--max-redirect`).
3. The Balancer (§5) computes connection allocation based on the mode (A/B/C), including allocation-time reallocation for files without range support.
4. Show the confirmation prompt (unless `-y`/`--quiet`/`--rpc`).
5. The scheduler runs tasks according to available parallel slots; tasks beyond the slot limit are queued.
6. Each task builds its chunk queue (§11.1) and spawns one worker goroutine per allocated connection to pull and download chunks.
7. The UI/RPC layer receives periodic progress updates from the aggregator.
8. Once all chunks of a task complete → verify checksum (if `--checksum` is set) → delete the `.odm` control file.
9. A final summary is shown (success/failure count, total time, average speed).

## 13. Non-Functional Requirements

- **Performance**: should saturate available bandwidth as long as the target server supports ranged requests; Balancer/UI overhead must be minimal (non-blocking relative to download I/O).
- **Portability**: single static Go binary, primary target Linux, but the design should not preclude cross-compiling to macOS/Windows.
- **Testability**: the Balancer logic (§5) must be a pure function that's easy to unit test across combinations of `C`, `N`, `SF`. Downloader integration tests should use a mock HTTP server (`httptest`).
- **Exit Codes** (for scripting):
  | Code | Meaning |
  |---|---|
  | `0` | All downloads succeeded |
  | `1` | General error / invalid argument |
  | `2` | Network error (all retries exhausted) |
  | `3` | Partial failure in a batch (some files failed) |
  | `4` | Cancelled by the user |

## 14. Security

- TLS certificate verification is enabled by default (`--check-certificate=true`).
- The RPC server binds to `127.0.0.1` only by default; exposing it to the network (`--rpc-listen-all`) should be paired with `--rpc-secret`.
- The `/etc/odm/config.conf` file should be readable by regular users but writable only by root (standard `644`, owned by root).

## 15. Roadmap / Future Features (Post-MVP)

- **Mid-flight dynamic reallocation**: rebalance connections of segments/chunks that are already in progress (beyond the allocation-time reallocation already in MVP, §5.5) — requires live goroutine renegotiation, deferred due to complexity.
- **Multi-mirror download**: when several URLs are known to point to the *same* file, split chunks across all of them simultaneously (not just picking the single fastest one) — bigger scope change to the Balancer (needs a "URL group per file" concept), so kept out of MVP.
- **HTTP/2 / HTTP/3 support** — low priority, compatibility-fallback only. Note: this is deliberately *not* a core identity feature — ODM's whole value proposition is opening many independent TCP connections to work around per-connection throttling many servers/CDNs apply; HTTP/2 stream multiplexing over a single TCP connection would undercut that. Worth adding later only so ODM degrades gracefully against HTTP/2-only servers.
- Per-task speed limits (not just the global `--limit-rate` token bucket, §11.4).
- BitTorrent/magnet link support.
- Daemon mode + systemd unit file (`odm.service`).
- A reference Web UI (optional, separate from the core, built on the RPC layer).
- Distribution packages for Arch/CachyOS (`PKGBUILD`, AUR).

## 16. Acceptance Criteria

- [ ] Balancer Modes A, B, and C produce connection allocations exactly matching the formulas in §5, verified by unit tests.
- [ ] Total active connections never exceed `MaxConnections` (default 32) unless the user explicitly raises it via `--max-connections`, in which case a warning is shown.
- [ ] Chunk queue (§11.1) correctly redistributes work across workers when one worker is artificially slowed in a test — verified by an integration test comparing completion time against a static equal-split baseline.
- [ ] `--limit-rate` produces a stable aggregate speed close to the configured limit regardless of how many connections/tasks are active.
- [ ] The progress bar renders per the format in §8, including the `[x<N>]` per-file connection indicator, with correct non-TTY fallback.
- [ ] The RPC server is reachable by a simple external client (e.g. `curl` against the JSON-RPC endpoint) for `addUri`, `tellStatus`, and receives events over WebSocket.
- [ ] Resume (`--continue`) successfully continues an interrupted download without file corruption.
- [ ] Redirects (301/302/307/308) are followed up to the `--max-redirect` limit, with a clear error if the limit is exceeded.
- [ ] Batch URL parsing correctly handles both space-separated positional args and the legacy comma-separated single string (§6.4), including URLs containing literal commas when passed as separate positional args or via `-i`.

## 17. Development Phases (suggested for incremental implementation via Claude Code)

1. **Phase 1 — Single-File MVP**: basic CLI, HTTP client + redirects, range-support probe (§5.2), chunk-queue download engine (§11.1) for a single file (Mode A), basic progress bar, config file parsing.
2. **Phase 2 — Batch & Balancer**: full implementation of Modes B & C incl. allocation-time reallocation, scheduler/queue, confirmation prompt, multi-file pacman-style progress bar with color states, global token-bucket rate limiter.
3. **Phase 3 — Reliability**: resume (`.odm` control file, chunk-aware), retry/backoff, checksum verification, logging.
4. **Phase 4 — RPC**: JSON-RPC server + WebSocket events, secret-token authentication.
5. **Phase 5 — Polish**: comprehensive Balancer unit tests, documentation (`README.md`, man page), packaging.

---

*This document is meant as an implementation baseline. Technical details (function names, structs, etc.) can be adapted by Claude Code as long as the behavior specified in §5 (Balancer), §8 (UI), and §10 (RPC) stays consistent with this document.*