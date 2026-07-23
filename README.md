# ODM — Oryn Download Manager

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

## Configuration

Config source priority (`CLI flags` win; a defaulted flag never overwrites a value the user put in a file):

```
CLI args  >  ~/.config/odm/config.conf  >  /etc/odm/config.conf  >  defaults
```

The file is `key = value`, one per line, `#` for comments; key names match the CLI long-flags (without `--`). See [`configs/odm.conf.example`](configs/odm.conf.example) for a fully-commented template.

Key flags (`odm --help` for the full list):

```
-c, --connections N        total connection budget           (default 5)
    --max-connections N    soft ceiling; exceeding warns     (default 32)
    --split-file/-sf N     connections per file in batch     (unset = 1 each)
-o, --output NAME          output filename (single-file only)
-d, --dir PATH             destination directory             (default cwd)
-i, --input-file FILE     read URL list from FILE
-y, --yes                  skip the confirmation prompt
-q, --quiet                no progress bar (cron/scripts); also skips prompt
-x, --continue             resume incomplete file via .odm control file (default on)
    --chunk-size SIZE      work-stealing chunk size          (default 4M)
    --max-redirect N       redirect hops to follow            (default 5)
    --retry N              retries per segment                 (default 3)
    --retry-wait SEC       delay between retries             (default 2)
    --timeout SEC          dial+headers timeout              (default 30)
    --user-agent UA        custom User-Agent                 (default odm/<ver>)
-H, --header K:V           add a custom header (repeatable)
    --referer URL          set the Referer header
    --proxy URL            http/https/socks5 proxy
    --check-certificate    verify TLS                        (default true)
    --checksum algo:hash   verify md5/sha1/sha256
    --limit-rate RATE      global speed limit, e.g. 5M/500K
```

> **Comma note:** the legacy `"url1,url2,..."` single-argument form is still supported, but a comma *inside* a URL (e.g. `?ids=1,2,3`) is ambiguous, so **space-separated positional args** are the recommended form. URLs with a literal comma must use either space-separated form or `-i <file>`.

---

## Rate limiting

`--limit-rate` is enforced with a **global token bucket** shared across *all* active workers of *all* tasks, rather than splitting the limit per connection. Throttling happens at the data-stream level (bytes read from the network, before writing to disk), so the aggregate throughput stays close to the configured cap regardless of how many connections are alive or how the batch queue advances.

---

## Resume

With `-x`/`--continue` (on by default), each download writes a `<file>.odm` control file recording which chunks are already done. On re-run with the same destination present, only the un-finished chunks are re-fetched — no corruption, no restart. The control file is deleted on a clean completion.

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
| `odm.changeOption` | Change an option on a running task (MVP: acknowledged no-op; mid-flight mutation is a roadmap item). |
| `odm.getGlobalStat` | Global stats (active/waiting/stopped counts). |
| `odm.getVersion` | Version + enabled features. |
| `odm.shutdown` | Shut the daemon down. |

### Events (WebSocket `/ws`)

All five PRD §10.3 events are emitted over the `/ws` fan-out:

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

The behaviour in the PRD's §16 checklist is covered by the test suite:

- Balancer Modes A/B/C produce allocations exactly matching the formulas — `internal/scheduler/balancer_test.go`.
- Total active connections never exceed `--max-connections` (default 32) unless the user raises it — enforced in `Compute`, a warning is printed in that case.
- Chunk-queue work-stealing: an artificially-slowed worker beats a static equal-split baseline — `internal/download/manager_workstealing_test.go`.
- `--limit-rate` stable aggregate near the cap regardless of connection count — `internal/ratelimit` + integration in `internal/download`.
- Progress bar renders per the §8 format with the `[x<N>]` per-file indicator and a non-TTY fallback — `internal/ui/progress_test.go`.
- RPC `addUri`/`tellStatus` reachable via `curl`, all five §10.3 events (`onDownloadStart`/`Progress`/`Complete`/`Error`/`Pause`) received over a real `/ws` dial — `internal/rpc/server_test.go` (`TestServer_WSCompletionEvents`, `TestServer_WSErrorEvent`, `TestServer_WSDialEvent*`).
- Resume (`--continue`) continues an interrupted download without corruption — `internal/download/manager_test.go`.
- Redirects followed up to `--max-redirect` — `internal/transport/transport_test.go`.
- Batch URL parsing (space-separated, legacy comma, URLs with literal commas, `-i`) — `internal/config/config_test.go`.

Run them all:

```bash
go test ./...
```

---

## License

MIT. See [`LICENSE`](LICENSE).
