# ODM — Oryn Download Manager

[![CI](https://github.com/Fahry-a/ODM/actions/workflows/ci.yml/badge.svg)](https://github.com/Fahry-a/ODM/actions/workflows/ci.yml)
[![Release](https://github.com/Fahry-a/ODM/actions/workflows/release.yml/badge.svg)](https://github.com/Fahry-a/ODM/actions/workflows/release.yml)
[![AUR](https://img.shields.io/aur/version/odm-bin?label=AUR)](https://aur.archlinux.org/packages/odm-bin)

`odm` is a CLI download manager written in Go, inspired by [`aria2c`](https://aria2.github.io/). It ships as a single static binary with no runtime dependencies.

Its core differentiator is the **Connection Balancer** — automatic allocation of parallel connections that adapts between *single-file* and *many-files* (batch) modes, so you set one connection budget and the tool splits it sensibly across files instead of you computing connections-per-file by hand.

It also has a pacman/CachyOS-style (`ILoveCandy`) progress bar, and a JSON-RPC 2.0 + WebSocket RPC server so other programs (CLIs, GUIs, scripts) can drive it — the same relationship `aria2c` has with `AriaNg`.

---

## Install

From source (requires Go 1.26+):

```bash
git clone <this repo> && cd odm
go build -o odm ./cmd/odm
# optional: install to PATH
install -Dm755 odm /usr/local/bin/odm
```

System-wide default config (root-owned, `0644`, per the security guidance):

```bash
install -Dm644 configs/odm.conf.example /etc/odm/config.conf
```

Per-user overrides go in `~/.config/odm/config.conf`.

---

## Quick start

The three canonical invocations (from the spec):

```bash
# 1. Single file, 16 parallel connections to one server for one file.
odm -c 16 https://files.test.xyz/file.tar.gz

# 2. Batch (no -sf): 16 URLs, 1 connection per file, 16 files run in parallel.
#    RECOMMENDED: space-separated positional args (comma-safe).
odm -c 16 https://files.test.xyz/file1.tar.gz url2 url3 ...   # 16 urls

# 3. Batch with -sf 4: 16 connections split 4 per file → 4 files run in parallel
#    (the rest queue automatically as slots free).
odm -c 16 -sf 4 https://files.test.xyz/file1.tar.gz url2 url3 ...
```

For large batches (>10 URLs) prefer an input file:

```bash
odm -i file-list.txt        # one URL per line, '#' comments and blanks skipped
```

---

## The Connection Balancer (`-c`, `-sf`)

`-c` is the **total** parallel-connection budget. What it controls depends on the mode:

| Mode | Condition | What `-c` controls | per-file connections |
|---|---|---|---|
| **A** | one URL (`-sf` ignored) | whole budget → the one file | `min(-c, --max-connections)` |
| **B** | many URLs, no `-sf` | how many files run **in parallel** | 1 each |
| **C** | many URLs + `-sf N` | `floor(-c / N)` files run in parallel | `N` (remainder distributed to the first files) |

`--max-connections` (default **32**) is a soft ceiling: going above it just prints a warning (`connections above 32 may get throttled/blocked by some servers`), since many CDN/servers treat >~30 concurrent connections from one IP as abusive. Power users on a LAN/local CDN can raise it.

Files that don't support HTTP range requests (the probe falls back through HEAD → ranged GET → single-stream) are automatically given a single connection, and the freed budget is redistributed to the other files in the same scheduling pass. (Mid-flight dynamic rebalancing of *already-downloading* chunks is a roadmap item, not in this release.)

### Why a chunk queue (work-stealing)

Each file is split into small chunks (default `--chunk-size 4M`) pushed into a shared per-file queue. Worker goroutines — one per allocated connection — pull the next chunk as soon as they finish the previous one. This avoids the classic **straggler problem** of a static equal-split: a slow connection just processes fewer chunks instead of holding the whole file back.

---

## Downloader profiles (`--profile`)

`odm` ships four engine profiles that change how a file is fetched. Pick one
with `--profile NAME` (or `profile = NAME` in the config file). The default
is `odm`.

| Profile | Engine | When to use |
|---|---|---|
| `odm` (default) | fixed chunks, work-stealing, HTTP/1.1 multi-connection | the default; best with range-capable servers |
| `aria2c` | static equal split into `--split` segments, HTTP/2 streams over one connection | large files on HTTPS servers; mimics aria2c's `-s`/`-x` model |
| `both` | 50/50 split: first half via the odm engine, second half via the aria2c engine | very large files where you want both engines at once |
| `smart` | auto-picks per file after probing (range support, size, h2 readiness) | set it and forget it |

### Profile flags

- `--split N` — aria2c/both: number of segments per file (default `5`).
- `--min-split-size SIZE` — aria2c/both: don't split ranges smaller than 2×
  this (default `20M`); a file below that downloads as a single segment.
- `--max-connection-per-server N` — aria2c: per-server connection cap when
  the server falls back to HTTP/1.1 (default `1`). Under HTTP/2 all streams
  share one connection, so the cap is irrelevant there.

### How each profile fetches

**`odm`** splits the file into fixed `--chunk-size` chunks (default 4 MiB)
pushed into a shared work-stealing queue; `-c` workers pull the next chunk as
soon as they finish, one HTTP/1.1 connection each. This is the multi-connection
aggregation ODM is built around.

**`aria2c`** divides the file into `--split` (default 5) roughly-equal
segments — one per worker — and speaks HTTP/2, so all segments multiplex over
a single TCP connection (the aria2c model where `-c` means concurrent h2
streams, not TCP connections). A failed segment is retried by the same worker
(no work-stealing). On `http://` URLs or h1-only servers Go falls back to
HTTP/1.1 automatically.

**`both`** runs the two engines side by side: region 1 `[0, mid)` uses the odm
engine (work-stealing chunks, HTTP/1.1), region 2 `[mid, end)` uses the aria2c
engine (static segments, HTTP/2). Files under 4 MiB degrade to the plain odm
engine.

**`smart`** probes each URL (range support, size, h2 readiness) and picks the
engine per file: no-range / sizeless / small (< 8 MiB) / no-h2 / low
connections → `odm`; ≥ 256 MiB with ≥ 6 connections → `both`; otherwise →
`aria2c`.

### Examples

```bash
# aria2c-style: 5 segments over h2 streams (HTTPS server)
odm --profile aria2c -c 5 https://files.test.xyz/big.iso

# both engines at once on a big file: odm half + aria2c half
odm --profile both -c 8 https://files.test.xyz/huge.iso

# smart: let odm decide per file (batch)
odm --profile smart -c 16 -i file-list.txt

# tune the aria2c split (4 segments, don't split below 10 MiB)
odm --profile aria2c --split 4 --min-split-size 10M https://files.test.xyz/big.iso
```

> **Note:** HTTP/2 negotiation needs HTTPS (ALPN). An `http://` URL or an
> h1-only server makes the h2 profiles fall back to HTTP/1.1 automatically.
> ODM's default `odm` profile deliberately disables HTTP/2 so `-c` always
> means N real TCP connections — the multi-connection aggregation is the
> point of the tool.

---

## Configuration

Config source priority (`CLI flags` win; a defaulted flag never overwrites a value the user put in a file):

```
CLI args  >  ~/.config/odm/config.conf  >  /etc/odm/config.conf  >  defaults
```

The file is `key = value`, one per line, `#` for comments; key names match the CLI long-flags (without `--`). See [`configs/odm.conf.example`](configs/odm.conf.example) for a fully-commented template.

Key flags (`odm --help` for the full list):

```
-c, --connections N        total connection budget           (default 5)
-m, --max-connections N    soft ceiling; exceeding warns     (default 32)
    --split-file/-sf N     connections per file in batch     (unset = 1 each)
-o, --output NAME          output filename (single-file only)
-d, --dir PATH             destination directory             (default cwd)
-i, --input-file FILE     read URL list from FILE
-y, --yes                  skip the confirmation prompt
    --dry-run              probe + show the plan, download nothing
-q, --quiet                no progress bar (cron/scripts); also skips prompt
-x, --continue             resume incomplete file via .odm control file (default on)
    --auto-rename          existing destination → name.<N>.ext
    --skip-existing         skip files present with a matching size
    --session-log FILE     JSONL progress/summary events (wrappers/GUIs)
-s, --chunk-size SIZE      work-stealing chunk size          (default 4M)
-n, --max-redirect N       redirect hops to follow            (default 5)
-r, --retry N              retries per segment                 (default 3)
-w, --retry-wait SEC       delay between retries             (default 2)
-t, --timeout SEC          dial+headers timeout              (default 30)
-u, --user-agent UA        custom User-Agent                 (default odm/<ver>)
-H, --header K:V           add a custom header (repeatable)
    --load-cookies FILE    load Netscape cookies.txt as a Cookie header
    --referer URL          set the Referer header
-p, --proxy URL            http/https/socks5 proxy
    --check-certificate    verify TLS                        (default true)
    --checksum algo:hash   verify md5/sha1/sha256
    --checksum-url URL     fetch the checksum from a sidecar URL
    --mirror URL           alternate URL for the same file (repeatable)
-l, --limit-rate RATE      global speed limit, e.g. 5M/500K
    --limit-rate-per-task RATE  per-task speed cap (stacked on global), e.g. 2M
-L, --log FILE             mirror logs to FILE
```

> **Comma note:** each positional argument is one URL — commas inside a URL (e.g. `?ids=1,2,3`) are literal content. For long lists use `-i <file>` (one URL per line).

---

## Rate limiting

`--limit-rate` is enforced with a **global token bucket** shared across *all* active workers of *all* tasks, rather than splitting the limit per connection. Throttling happens at the data-stream level (bytes read from the network, before writing to disk), so the aggregate throughput stays close to the configured cap regardless of how many connections are alive or how the batch queue advances.

An optional **per-task cap** (`--limit-rate-per-task`) can be stacked on top: each task gets its own token bucket, and the body is throttled through both the per-task and global buckets. The effective speed of a single task is `min(per-task, global)`, and the total across all tasks never exceeds the global cap. Both limits can be changed at runtime via RPC `odm.changeOption`.

`--limit-rate` and `--limit-rate-per-task` both support human-readable suffixes: `5M`, `500K`, `2.5G`, `off` to disable.

When a server starts throttling you (`HTTP 429`), ODM reacts on its own: the global rate halves (floor 64 KiB/s) so every connection eases off together, and the first successful chunk afterwards restores your configured cap.

---

## Mirrors & verification

**`--mirror URL`** (repeatable) registers alternate sources for the *same* file. Each chunk request rotates across the primary URL and every mirror, so a slow or throttling source simply serves fewer chunks while the rest keep full speed. Per-chunk `Content-Range` validation applies to each source independently — a misbehaving mirror fails its own chunks instead of corrupting the file. Mirrors are assumed byte-identical.

```bash
odm -c 16 https://primary.example/big.iso --mirror https://mirror1.example/big.iso --mirror https://mirror2.example/big.iso
```

**Verification** comes in three forms: explicit (`--checksum sha256:<hex>`), fetched from a sidecar (`--checksum-url https://host/file.sha256` accepts sha256sum/md5sum-style files or a bare hash), or embedded in a Metalink4 document. A checksum mismatch fails the task rather than keeping silently-corrupt output.

**Metalink4**: `-i file.meta4` reads an RFC 5854 document — the mirror URLs become download targets (first = primary, rest feed `--mirror`) and the strongest listed hash becomes the checksum automatically:

```bash
odm -c 16 -i release.meta4
```

---

## Resume & collisions

With `-x`/`--continue` (on by default), each download writes a `<file>.odm` control file recording which chunks are already done. On re-run with the same destination present, only the un-finished chunks are re-fetched — no corruption, no restart. The control file is deleted on a clean completion. Resumed downloads also send the stored ETag as `If-Range`: if the remote file changed since the interruption, ODM detects it and restarts cleanly instead of stitching old bytes to new ones.

For an existing destination without a control file, two collision policies exist (mutually exclusive):

- `--auto-rename` — save as `name.<N>.ext` (lowest free counter); the existing file is never touched.
- `--skip-existing` — skip when a file of the same size is already there; a size mismatch warns and re-downloads.

---

## Automation hooks

- **`--dry-run`** — probe every URL, show the balancer's plan (mode, per-file connections, queue marks, total size), exit 0 without downloading.
- **`--session-log FILE`** — append JSONL events (`progress` per task snapshot, closing `summary`) so wrappers/GUIs get a machine-readable feed without parsing terminal output; pairs naturally with the RPC server for full control.

---

## RPC server (`--rpc`)

Run ODM as a daemon exposing JSON-RPC 2.0 + WebSocket, exactly so third-party GUIs/scripts can be built on top without parsing terminal output:

```
POST  http://127.0.0.1:6900/rpc   (JSON-RPC 2.0)
 WS   ws://127.0.0.1:6900/ws      (event notifications)
```

Default bind is `127.0.0.1` (safe). `--rpc-listen-all` binds `0.0.0.0` — pair that with `--rpc-secret`. Auth is aria2-style: the first JSON-RPC param is `"token:<secret>"` (or `?secret=<value>` on the WebSocket upgrade).

### Quickstart with `curl`

```bash
odm --rpc --rpc-secret hunter2 --rpc-listen-port 6900 -d ~/Downloads &

# add a download; note the token:<secret> first param
curl -s -X POST http://127.0.0.1:6900/rpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"odm.addUri","params":["token:hunter2","https://files.test.xyz/x.tar.gz"],"id":1}'
# → {"jsonrpc":"2.0","result":"odm-001","id":1}

# ask its status
curl -s -X POST http://127.0.0.1:6900/rpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"odm.tellStatus","params":["token:hunter2","odm-001"],"id":2}'
```

### Methods

| Method | Description |
|---|---|
| `odm.addUri` | Add a single URL to the queue. |
| `odm.addBatch` | Add many URLs at once. |
| `odm.pause` / `odm.pauseAll` | Pause a task / all tasks. |
| `odm.unpause` / `odm.unpauseAll` | Resume paused task(s). |
| `odm.remove` | Cancel and remove a task. |
| `odm.tellStatus` | Detailed status of one task (progress, speed, conns, ETA). |
| `odm.tellActive` / `odm.tellWaiting` / `odm.tellStopped` | List active / queued / finished tasks. |
| `odm.changeOption` | Change options at runtime: `max-download-limit` (global rate), `max-download-limit-per-task` (per-task rate), `connections` (mid-flight reallocation). |
| `odm.getGlobalStat` | Global stats (active/waiting/stopped counts). |
| `odm.getVersion` | Version + enabled features. |
| `odm.shutdown` | Shut the daemon down. |

### Events (WebSocket `/ws`)

All five WebSocket events are emitted over the `/ws` fan-out:

| Event | When |
|---|---|
| `onDownloadStart` | a task is added via `odm.addUri` |
| `onDownloadProgress` | periodic snapshot (bytes done, speed, ETA) — throttled to ~250ms |
| `onDownloadComplete` | a task finishes cleanly |
| `onDownloadError` | a task fails / is cancelled |
| `onDownloadPause` | a task is paused via `odm.pause` |

Each event's `params` carries the same field set as `odm.tellStatus`'s result.

---

## Exit codes

| Code | Meaning |
|---|---|
| `0` | All downloads succeeded |
| `1` | General error / invalid argument |
| `2` | Network error (all retries exhausted) |
| `3` | Partial failure in a batch (some files failed) |
| `4` | Cancelled by the user |

---

## Acceptance

The acceptance checklist is covered by the test suite:

- Balancer Modes A/B/C produce allocations exactly matching the formulas — `internal/scheduler/balancer_test.go`.
- Total active connections never exceed `--max-connections` (default 32) unless the user raises it — enforced in `Compute`, a warning is printed in that case.
- Chunk-queue work-stealing: an artificially-slowed worker beats a static equal-split baseline — `internal/download/manager_workstealing_test.go`.
- `--limit-rate` stable aggregate near the cap regardless of connection count — `internal/ratelimit` + integration in `internal/download`.
- Progress bar renders per the pacman format with the `[x<N>]` per-file indicator and a non-TTY fallback — `internal/ui/progress_test.go`.
- RPC `addUri`/`tellStatus` reachable via `curl`, all five RPC events (`onDownloadStart`/`Progress`/`Complete`/`Error`/`Pause`) received over a real `/ws` dial — `internal/rpc/server_test.go` (`TestServer_WSCompletionEvents`, `TestServer_WSErrorEvent`, `TestServer_WSDialEvent*`).
- Resume (`--continue`) continues an interrupted download without corruption — `internal/download/manager_test.go`.
- Redirects followed up to `--max-redirect` — `internal/transport/transport_test.go`.
- Batch URL parsing (space-separated, URLs with literal commas, `-i`) — `internal/config/config_test.go`.
- Dead links fail fast: a non-retryable 4xx chunk errors after one attempt instead of burning the retry budget — `internal/download/permanent_test.go`.
- Resume drift detection via `If-Range` — a changed remote restarts cleanly instead of stitching old bytes to new — `internal/download/permanent_test.go`.
- Mirror rotation spreads chunks across every source; assembled bytes match any single source — `internal/download/mirror_test.go`.
- Collision policies (`--auto-rename`/`--skip-existing`) never touch a pre-existing file and re-download on size mismatch — `internal/download/collision_test.go`.
- Cookies ride the header pipeline into every request and never leak into the `.odm` control file — `internal/download/cookie_test.go`, `internal/config/config_test.go`.
- Metalink4 input parses URLs + strongest hash into the plan — `internal/config/config_test.go`.
- Adaptive slowdown halves the global rate on 429 and restores the configured cap afterwards — `internal/ratelimit/bucket_test.go`.

Run them all:

```bash
go test ./...
```

---

## License

MIT. See [`LICENSE`](LICENSE).
