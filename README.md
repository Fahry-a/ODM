# ODM — Oryn Download Manager

[![CI](https://github.com/Fahry-a/ODM/actions/workflows/ci.yml/badge.svg)](https://github.com/Fahry-a/ODM/actions/workflows/ci.yml)
[![Release](https://github.com/Fahry-a/ODM/actions/workflows/release.yml/badge.svg)](https://github.com/Fahry-a/ODM/actions/workflows/release.yml)
[![AUR](https://img.shields.io/aur/version/odm-bin?label=AUR)](https://aur.archlinux.org/packages/odm-bin)

CLI download accelerator written in Go, inspired by [aria2c](https://aria2.github.io/). Single static binary, zero runtime dependencies.

The **Connection Balancer** automatically splits a single `-c` budget across files — one-file mode uses all connections on the file, batch mode distributes evenly — so you never compute connections-per-file by hand.

Also ships a pacman-style (`ILoveCandy`) progress bar and a JSON-RPC 2.0 + WebSocket daemon for third-party GUIs and scripts.

---

## Features

- **Connection Balancer** — set `-c N` and the tool figures out per-file allocation
- **Work-stealing chunk queue** — slow connections process fewer chunks instead of holding the file back
- **4 engine profiles** — `odm` (multi-TCP), `aria2c` (h2 streams), `both` (hybrid), `smart` (auto-pick)
- **Resume** — `.odm` control file, ETag drift detection, per-chunk SHA-256 integrity
- **Mirrors & Metalink4** — rotate chunks across alternate sources, auto-verify checksums
- **Rate limiting** — global + per-task token buckets, adaptive 429 back-off, runtime `changeOption`
- **Collision handling** — `--auto-rename` / `--skip-existing`
- **RPC daemon** — JSON-RPC 2.0 + WebSocket, TLS support, aria2-style token auth
- **Pacman progress bar** — responsive, CJK-aware, ANSI color, non-TTY fallback

---

## Installation

### One-line install

```bash
curl -fsSL https://odm.orynix.id/install.sh | sh
```

Auto-detects prefix: writable `/usr/local` → system-wide, otherwise falls back to `~/.local` (no sudo). Also installs man page and config. Options: `--version X.Y.Z`, `--prefix /path`, `-y` (skip prompt).

### Arch Linux (AUR)

```bash
# Pre-built binary (recommended)
yay -S odm-bin

# Or build from source
yay -S odm
```

### Pre-built binaries

Download from [GitHub Releases](https://github.com/Fahry-a/odm/releases). Available for:

| Platform | Architecture |
|----------|-------------|
| Linux    | amd64, i386, arm, arm64 |
| macOS    | amd64, arm64 |

```bash
# Example: Linux amd64
curl -LO https://github.com/Fahry-a/odm/releases/latest/download/odm_1.7.0_linux_amd64.tar.gz
tar -xzf odm_*.tar.gz
sudo install -Dm755 odm /usr/local/bin/odm
```

### From source

Requires **Go 1.26+**.

```bash
git clone https://github.com/Fahry-a/odm.git && cd odm
go build -o odm ./cmd/odm
sudo install -Dm755 odm /usr/local/bin/odm
```

### Systemd daemon

The AUR package installs a systemd unit. For manual installs:

```bash
sudo install -Dm644 packaging/odm.service /usr/lib/systemd/system/odm.service
sudo systemctl daemon-reload
sudo systemctl enable --now odm
```

The unit runs `odm --rpc` with `DynamicUser=yes` and hardening applied. Configure via `/etc/odm/odm.env` (environment file) or `/etc/odm/config.conf`.

### Man page

```bash
sudo install -Dm644 docs/odm.1 /usr/share/man/man1/odm.1
man odm
```

---

## Quick start

```bash
# Single file, 16 parallel connections
odm -c 16 https://example.com/big.iso

# Batch: 1 connection per file, files run in parallel
odm -c 16 https://example.com/a.tar.gz https://example.com/b.tar.gz https://example.com/c.tar.gz

# Batch with explicit split: 4 connections per file, 4 files at a time
odm -c 16 -sf 4 https://example.com/a.tar.gz https://example.com/b.tar.gz ...

# From an input file (one URL per line, '#' comments and blanks skipped)
odm -i urls.txt
```

---

## Connection Balancer (`-c`, `-sf`)

`-c` is the **total** parallel-connection budget. How it's used depends on the mode:

| Mode | Condition | What `-c` controls | Per-file connections |
|------|-----------|-------------------|---------------------|
| **A** | one URL | whole budget → the one file | `min(-c, --max-connections)` |
| **B** | many URLs, no `-sf` | how many files run **in parallel** | 1 each |
| **C** | many URLs + `-sf N` | `floor(-c / N)` files in parallel | N (remainder to first files) |

`--max-connections` (default 32) is a soft ceiling — exceeding it prints a warning. Files without HTTP range support get 1 connection; the freed budget is redistributed to the other files.

### Why work-stealing

Each file is split into chunks (`--chunk-size 4M` default) pushed into a per-file queue. Worker goroutines pull the next chunk as soon as they finish the previous one. A slow connection just processes fewer chunks instead of stalling the whole file.

---

## Engine profiles (`--profile`)

| Profile | Engine | When to use |
|---------|--------|-------------|
| `odm` (default) | Fixed chunks, work-stealing, HTTP/1.1 | Default; best with range-capable servers |
| `aria2c` | Static equal split, HTTP/2 streams | Large files on HTTPS; mimics aria2c `-s`/`-x` |
| `both` | odm (region 1) + aria2c (region 2) | Very large files, both engines at once |
| `smart` | Auto-picks per file after probing | Set it and forget it |

### Profile flags

- `--split N` — aria2c/both: segments per file (default 5)
- `--min-split-size SIZE` — aria2c/both: don't split ranges < 2x this (default 20M)
- `--max-connection-per-server N` — aria2c: per-server h1 cap (default 1); irrelevant under h2

### How each profile works

**`odm`** splits into fixed `--chunk-size` chunks in a work-stealing queue; `-c` workers grab the next chunk over individual HTTP/1.1 connections. This is the multi-connection aggregation ODM is built around.

**`aria2c`** divides into `--split` roughly-equal segments, one per worker, multiplexed over HTTP/2. A failed segment retries in the same worker (no stealing). Falls back to HTTP/1.1 on plain `http://` or h1-only servers.

**`both`** runs both engines side by side: region 1 `[0, mid)` uses odm/h1, region 2 `[mid, end)` uses aria2c/h2. Files under 4 MiB degrade to plain odm.

**`smart`** probes each URL and picks: no-range / sizeless / small / no-h2 / low conns → `odm`; ≥256 MiB + ≥6 conns → `both`; otherwise → `aria2c`.

### Examples

```bash
odm --profile aria2c -c 5 https://example.com/big.iso
odm --profile both -c 8 https://example.com/huge.iso
odm --profile smart -c 16 -i file-list.txt
odm --profile aria2c --split 4 --min-split-size 10M https://example.com/big.iso
```

> **Note:** HTTP/2 needs HTTPS (ALPN). `http://` URLs or h1-only servers fall back to HTTP/1.1 automatically.

---

## Configuration

Config priority (a defaulted flag never overwrites a user-set value):

```
CLI args  >  ~/.config/odm/config.conf  >  /etc/odm/config.conf  >  defaults
```

Format is `key = value`, one per line, `#` for comments. Key names match CLI long-flags without `--`. See [`configs/odm.conf.example`](configs/odm.conf.example) for a fully-commented template.

### Common flags

| Flag | Description | Default |
|------|-------------|---------|
| `-c`, `--connections N` | Total connection budget | 5 |
| `-m`, `--max-connections N` | Soft ceiling (warns above) | 32 |
| `--split-file`, `-sf N` | Connections per file in batch | 1 each |
| `-o`, `--output NAME` | Output filename (single-file only) | — |
| `-d`, `--dir PATH` | Destination directory | cwd |
| `-i`, `--input-file FILE` | Read URLs from file | — |
| `-y`, `--yes` | Skip confirmation prompt | — |
| `--dry-run` | Probe + show plan, download nothing | off |
| `-q`, `--quiet` | No progress bar (cron/scripts); skips prompt | off |
| `-x`, `--continue` | Resume via `.odm` control file | on |
| `--auto-rename` | Save as `name.<N>.ext` on collision | off |
| `--skip-existing` | Skip files present with matching size | off |
| `--session-log FILE` | JSONL progress/summary events | — |
| `-s`, `--chunk-size SIZE` | Work-stealing chunk size | 4M |
| `-n`, `--max-redirect N` | Redirect hops | 5 |
| `-r`, `--retry N` | Retries per segment | 3 |
| `-w`, `--retry-wait SEC` | Delay between retries | 2 |
| `-t`, `--timeout SEC` | Dial + headers timeout | 30 |
| `-u`, `--user-agent UA` | Custom User-Agent | odm/\<ver\> |
| `-H`, `--header K:V` | Add custom header (repeatable) | — |
| `--load-cookies FILE` | Load Netscape cookies.txt | — |
| `--referer URL` | Set Referer header | — |
| `-p`, `--proxy URL` | http/https/socks5 proxy | — |
| `--check-certificate` | Verify TLS | true |
| `--checksum ALGO:HASH` | Verify md5/sha1/sha256 | — |
| `--checksum-url URL` | Fetch checksum from sidecar URL | — |
| `--mirror URL` | Alternate source (repeatable) | — |
| `-l`, `--limit-rate RATE` | Global speed limit (e.g. 5M) | off |
| `--limit-rate-per-task RATE` | Per-task speed cap | off |
| `-L`, `--log FILE` | Mirror logs to file | — |

> **Comma note:** each positional argument is one URL. Commas inside URLs (e.g. `?ids=1,2,3`) are literal. For long lists, use `-i <file>`.

---

## Rate limiting

`--limit-rate` uses a **global token bucket** shared across all workers of all tasks. Throttling happens at the data-stream level (bytes read from network, before writing to disk).

Optional `--limit-rate-per-task` adds a per-task bucket stacked on top. Effective single-task speed = `min(per-task, global)`. Both can be changed at runtime via RPC `odm.changeOption`.

Suffixes: `5M`, `500K`, `2.5G`, `off` to disable.

On HTTP 429, ODM halves the global rate automatically; the first healthy chunk after a cooldown restores your configured cap.

---

## Mirrors & verification

**Mirrors** (`--mirror URL`, repeatable) rotate chunk requests across all sources. A slow/throttled source simply serves fewer chunks. Per-chunk `Content-Range` validation applies independently per source.

```bash
odm -c 16 https://primary.example/big.iso \
    --mirror https://mirror1.example/big.iso \
    --mirror https://mirror2.example/big.iso
```

**Verification** — three forms:

- Explicit: `--checksum sha256:<hex>`
- Sidecar: `--checksum-url https://host/file.sha256` (sha256sum-style or bare hash)
- Metalink4: `-i file.meta4` — mirrors become download targets, strongest hash auto-selected

```bash
odm -c 16 -i release.meta4
```

A checksum mismatch fails the task — no silent corruption.

---

## Resume & collisions

With `--continue` (on by default), each download writes a `<file>.odm` control file tracking completed chunks. On re-run, only unfinished chunks are re-fetched. The stored ETag is sent as `If-Range` — if the remote file changed, ODM restarts cleanly instead of stitching old bytes to new.

Collision policies (mutually exclusive):

- `--auto-rename` — save as `name.<N>.ext` (lowest free counter), existing file untouched
- `--skip-existing` — skip when same-size file is present, warn and re-download on mismatch

---

## Automation hooks

- **`--dry-run`** — probe every URL, show the balancer's plan (mode, per-file connections, total size), exit 0
- **`--session-log FILE`** — append JSONL events (`progress` snapshots, closing `summary`) for wrappers/GUIs

---

## RPC server (`--rpc`)

JSON-RPC 2.0 + WebSocket daemon for third-party GUIs and scripts:

```
POST  http://127.0.0.1:6900/rpc   (JSON-RPC 2.0)
 WS   ws://127.0.0.1:6900/ws      (event push)
```

Default bind: `127.0.0.1`. `--rpc-listen-all` binds `0.0.0.0` — pair with `--rpc-secret`. TLS via `--rpc-tls-cert` + `--rpc-tls-key`.

Auth is aria2-style: first param is `"token:<secret>"` (or `?secret=<value>` on WebSocket upgrade).

### Quickstart

```bash
odm --rpc --rpc-secret hunter2 -d ~/Downloads &

# Add a download
curl -s -X POST http://127.0.0.1:6900/rpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"odm.addUri","params":["token:hunter2","https://example.com/x.tar.gz"],"id":1}'

# Check status
curl -s -X POST http://127.0.0.1:6900/rpc -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"odm.tellStatus","params":["token:hunter2","odm-001"],"id":2}'
```

### Methods

| Method | Description |
|--------|-------------|
| `odm.addUri` | Add a single URL to the queue |
| `odm.addBatch` | Add many URLs at once |
| `odm.pause` / `odm.pauseAll` | Pause a task / all tasks |
| `odm.unpause` / `odm.unpauseAll` | Resume paused task(s) |
| `odm.remove` | Cancel and remove a task |
| `odm.tellStatus` | Detailed status (progress, speed, conns, ETA) |
| `odm.tellActive` / `tellWaiting` / `tellStopped` | List active / queued / finished tasks |
| `odm.changeOption` | Runtime changes: `max-download-limit`, `connections`, etc. |
| `odm.getGlobalStat` | Global stats (active/waiting/stopped counts) |
| `odm.getVersion` | Version + features |
| `odm.shutdown` | Shut the daemon down |

### WebSocket events (`/ws`)

| Event | When |
|-------|------|
| `onDownloadStart` | Task added via `odm.addUri` |
| `onDownloadProgress` | Periodic snapshot (~250ms throttle) |
| `onDownloadComplete` | Task finishes cleanly |
| `onDownloadError` | Task fails / cancelled |
| `onDownloadPause` | Task paused via `odm.pause` |

Each event's `params` carries the same fields as `odm.tellStatus` result.

---

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | All downloads succeeded |
| 1 | General error / invalid argument |
| 2 | Network error (all retries exhausted) |
| 3 | Partial failure in batch |
| 4 | Cancelled by user |

---

## License

MIT. See [LICENSE](LICENSE).
