# AGENTS.md

## Build & test

```bash
go build ./...                    # build all packages
go build -o odm ./cmd/odm          # build the binary (like CI does)
go vet ./...                       # vet (CI runs this)
go test ./...                      # quick test
go test -race -count=1 ./...       # CI's exact test command (use this before committing)
go test -run TestName ./internal/ui/  # single test
go test ./internal/download/        # single package
```

CI (`.github/workflows/ci.yml`) runs: `go mod download` → `go build` → `go vet` → `golangci-lint` → `go test -race -count=1`. All must pass on push to `main`.

Linter: golangci-lint v2 with `.golangci.yml` config. `golangci-lint run --timeout=5m` must report 0 issues. ST1012 (error vars must be ErrFoo) is disabled — `NoControlFile` is intentionally readable. `errcheck` excludes `defer x.Close()` and `fmt.Fprint*` (best-effort CLI output). No Makefile — use `go` directly.

## Architecture

Single binary `cmd/odm/main.go` wires together internal packages. Two execution modes: **CLI one-shot** (default) and **RPC daemon** (`--rpc`).

```
cmd/odm         → entry point, flag parsing, mode dispatch
internal/config → CLI flags (pflag) + config file merge
internal/version → single source of truth for the Version const
internal/download → Task lifecycle, chunk queue (work-stealing), Manager, exit codes
internal/transport → HTTP client, Probe (HEAD → ranged GET → single-stream fallback), proxy (http/https/socks5/socks5h)
internal/scheduler → Connection Balancer (allocation modes A/B/C), Daemon (RPC), Queue
internal/ui → Pacman/ILoveCandy progress bar, confirmation prompt, terminal sizing
internal/rpc → JSON-RPC 2.0 + WebSocket server, Broadcaster
internal/ratelimit → Global token bucket (--limit-rate)
internal/storage → File WriteAt, resume control files (.odm, per-chunk hashes)
internal/logging → Leveled logger (--log / --log-level)
```

## Key conventions

- **Module path is `odm`** (not a GitHub URL). Imports look like `odm/internal/config`. Keep it this way.
- **Version is single-sourced** in `internal/version/version.go`, imported directly by `config` (default User-Agent), `download` (control-file metadata) and `rpc` (getVersion) — no aliases, so nothing can drift. The only remaining release risk is `packaging/PKGBUILD` `pkgver`, which CI checks stays in sync.
- **Progress bar format**: `internal/ui/render.go` is the pure rendering layer (`Bar()`, `BarIndeterminate()`, `renderTaskLine()`). `internal/ui/progress.go` owns the stateful `Renderer` (cache, animation tick, TTY redraw loop). Keep `render.go` functions pure/testable — animation state lives in `progress.go`.
- **Bar width is fixed** at 30 cells (`BarWidth` const). Dots are spaced (`o o o`), face animates `c`↔`C` every ~1s. `barLine()` pads to exactly `width` so the `%` column never shifts. `TestRenderTaskLine_FixedColumns` pins this.
- **ETA format is `HH:MM:SS`** (8 cells, `colETA=8`). `FormatDuration` caps at `99:59:59`. The download engine's `estimateETA` (`task.go`) must NOT multiply by `time.Second` a second time — the inner expression already yields nanoseconds.
- **`Snapshot()` is the read API** for task state. UI and RPC both read `download.ProgressView` via `Task.Snapshot()`. Write path is atomic fields (`conns`, `speed`, `bytesDone`, `state`).
- **Exit codes**: `0`=ok, `1`=args, `2`=network, `3`=partial, `4`=cancelled (`download.Exit*` constants).

## Testing quirks

- `internal/rpc` tests start real httptest servers with real downloads; they take several seconds under `-race`. `TestServer_AddURIAndTellActive` polled only `tellActive`/`tellWaiting` and was flaky because the task (an unresolvable `example.invalid`) can move from waiting to stopped between 20ms polls; it now also polls `tellStopped` and is deterministic.
- `internal/download` tests use httptest servers with real chunk downloads; they take several seconds under `-race`.
- `internal/storage` tests use `t.TempDir()` and concurrent goroutines to stress `WriteAt` non-overlap.
- `internal/logging` tests swap the unexported `from`/`file` fields to capture output in a buffer (same-package test access).
- UI tests inject a fake clock (`nowFn` in `clock.go`) — restore `nowFn` in `t.Cleanup` if you change it.

## Cross-compiling

```bash
export CGO_ENABLED=0 GOFLAGS="-trimpath -mod=readonly" GOTOOLCHAIN=local
LDFLAGS="-s -w -buildid="
GOOS=linux GOARCH=amd64 go build -ldflags="$LDFLAGS" -o build/odm_1.0.0_linux_amd64 ./cmd/odm
# also: 386, arm, arm64, darwin/amd64, darwin/arm64
```

`build/` is gitignored — output cross-compile binaries there.

## Releases

Automated end-to-end. Push a version bump to `main` → `auto-tag.yml` validates
and tags it → `release.yml` (tag push) verifies versions, cross-compiles, and
creates the GitHub Release → `aur-publish.yml` ships the AUR package.

**Release checklist (both must match the version being released):**

1. `internal/version/version.go:14` — `const Version = "odm/X.Y.Z"`
2. `packaging/PKGBUILD:9` — `pkgver=X.Y.Z`

No separate bump needed — `version.Version` is the only source, imported
directly by the packages that need it.

**Release steps (auto-tag flow):**

```bash
# 1. Bump the version in the 2 locations above
# 2. Move the [Unreleased] section in CHANGELOG.md → new "## [X.Y.Z] - YYYY-MM-DD"
# 3. Commit
git add -A && git commit -m "release: vX.Y.Z"
# 4. Push main only — auto-tag.yml creates the tag, then release + AUR follow
git push origin main
```

**Manual fallback** (auto-tag skipped or tag needs moving):

```bash
# Annotated tag with -m: a bare `git tag vX.Y.Z` opens $GIT_EDITOR (vi by
# default), which fails on machines without it.
git tag vX.Y.Z -m "release: vX.Y.Z"
git push origin main && git push origin vX.Y.Z
```

**What the workflows do:**

1. `auto-tag.yml` — on push to `main`: extracts the version from
   `internal/version/version.go` and `PKGBUILD`; FAILS if the two mismatch;
   creates an annotated tag only when the version is newer than the latest
   `v*` tag AND `CHANGELOG.md` has a `## [X.Y.Z]` header. Idempotent — a
   docs-only push is a no-op. Because a tag pushed with `GITHUB_TOKEN` cannot
   trigger other workflows (GitHub's recursion guard), it then dispatches
   `release.yml` explicitly via `workflow_dispatch`, targeting the tag ref so
   Release builds the exact tagged source (not main's latest); `release.yml`
   in turn dispatches `aur-publish.yml` explicitly (see item 3).
2. `release.yml` — on `v*` tag push: extracts version from the tag, re-checks
   both version files (fails on mismatch), requires the CHANGELOG entry
   (fails if missing), cross-compiles 6 targets, generates SHA-256 checksums,
   and creates the GitHub Release via `softprops/action-gh-release@v3`.
   Pre-release auto-detected if version contains `-` (e.g. `v0.2.0-rc1`).
3. `aur-publish.yml` — two modes:
   - **RELEASE** (dispatched explicitly by `release.yml` via
     `workflow_dispatch`; a `workflow_run` chain off a `GITHUB_TOKEN`-
     triggered release is suppressed by the recursion guard, so it is not a
     trigger): for new versions, bumps `pkgver`, resets `pkgrel` to 1, pulls
     the real binary checksums from the release, verifies via
     `makepkg --verifysource` (archlinux docker), publishes to AUR, and
     commits `PKGBUILD` back to `main`.
   - **REPUBLISH** (auto-triggered by a push to `packaging/PKGBUILD`): the
     version was already released, so it bumps `pkgrel` above upstream AUR
     (or keeps the local `pkgrel` when it is already ahead) and republishes.
     Skips silently when there is nothing to do — the
     version is untagged (new versions go through auto-tag → release.yml) or
     the local PKGBUILD is byte-identical to upstream AUR (defensive loop
     guard on top of GitHub's recursion guard, which already prevents
     `GITHUB_TOKEN` commit-backs from re-triggering).
   `.SRCINFO` is generated by the workflow at publish time and is not
   tracked. The re-push to `main` re-runs `auto-tag.yml`, which skips (tag
   already exists) — no loop.
4. `ci.yml` — build/vet/lint/test/race on every push + PR, and a version-
   consistency check (`internal/version/version.go` vs `pkgver`) so a partial
   bump can't land on `main`.

**Binary naming:** `odm_X.Y.Z_<os>_<arch>` (e.g. `odm_0.2.0_linux_amd64`)

**Manual release fallback (if needed):**

```bash
gh release create vX.Y.Z --prerelease build/odm_X.Y.Z_* --title "vX.Y.Z" --notes "..."
```

Tag format: `v<semver>`. Pre-releases use `--prerelease`.

## Push-to-main (user request)

When the user asks to "push to main" / "release" / "push ke github" (or similar),
do the full release in one flow — do NOT stop after committing:

1. **Bump the version** — semver, judged from the changes since the last `v*` tag:
   - new features (even small ones, e.g. chunk requeue, resume integrity checks) → **minor** bump (`1.1.0` → `1.2.0`)
   - bug fixes only → **patch** bump (`1.1.0` → `1.1.1`)
   - breaking change → **major** bump
   Check `git tag -l | sort -V | tail -1` for the last tag and
   `git log --oneline <last-tag>..HEAD` to see what changed. Update ALL TWO
   locations from the release checklist above, plus move the CHANGELOG
   `[Unreleased]` section under a new `## [X.Y.Z] - YYYY-MM-DD` header.
2. **Commit + push main only** — `git push origin main`. The `auto-tag.yml`
   workflow validates the two files + changelog and creates the tag itself.
3. **Verify the pipeline** — after pushing, confirm the tag was created and the
   release ran:
   ```bash
   git fetch --tags && git tag -l 'v*' | sort -V | tail -1
   gh run list --workflow=auto-tag.yml --limit 1
   gh run list --workflow=release.yml --limit 1
   gh release view "vX.Y.Z"
   ```
   If auto-tag skipped (e.g. the version was already tagged or the changelog
   entry was missing), fix and re-push, or tag manually.

If the version bump is ambiguous (e.g. the repo drifted), pick the most
conservative bump that the changelog supports and state the reasoning to the
user. Never push without the version + tag + changelog all aligned.

### PKGBUILD-only changes (republish) — do NOT bump anything yourself

For changes to an **already-released** version (`packaging/PKGBUILD` edits like
`pkgdesc`, `options`, dependencies, typos), leave `pkgver` and `pkgrel` alone.
The `aur-publish.yml` REPUBLISH mode handles the bump:

1. Edit `packaging/PKGBUILD` (do not touch `pkgver`/`pkgrel`).
2. Commit + push to `main`.
3. The workflow bumps `pkgrel = upstream + 1`, publishes to AUR, and commits the
   bumped PKGBUILD back to `main`. (If you bumped `pkgrel` yourself above
   upstream, it keeps yours — no double bump.)

Do NOT bump `pkgver` in PKGBUILD for a republish:

- An untagged version makes REPUBLISH skip silently — new versions are the
  release pipeline's job (auto-tag → release.yml → RELEASE mode, which resets
  `pkgrel` to 1).
- A `pkgver` bump without a matching `internal/version/version.go` bump fails
  `auto-tag.yml`'s consistency check on purpose.

Summary: **new version → bump `version.go` + `pkgver` + CHANGELOG yourself;
republish → edit PKGBUILD only and let the workflow bump `pkgrel`.**

## Roadmap (not yet implemented)

These are deferred — do NOT treat them as bugs:

- **Mid-flight dynamic reallocation of in-progress chunks** — rebalancing chunks
  already being downloaded (beyond the implemented connection-count adjustment
  via `AdjustConns`). Requires live goroutine renegotiation.
- **Multi-mirror download** — splitting chunks across duplicate URLs for the
  same file. Out of scope for the connection-aggregation value proposition.
- **BitTorrent / magnet links** — explicitly non-goal for MVP.
- **HTTP/2 / HTTP/3 stream multiplexing** — deliberately excluded; the whole
  point of ODM is multi-connection aggregation over HTTP/1.1. Future implementation
  if demand arises; would require a new Balancer mode that ignores connection
  budget for HTTP/2-capable servers.
- **`changeOption` beyond rate/connections** — implemented since 1.1.0:
  `max-download-limit`, `max-download-limit-per-task`, and `connections`
  (mid-flight). Other RPC spec mutations remain deferred.
- **Reference Web UI** — roadmap item built on the RPC + WebSocket layer.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **ODM** (1316 symbols, 4132 relationships, 114 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> Index stale? Run `node .gitnexus/run.cjs analyze` from the project root — it auto-selects an available runner. No `.gitnexus/run.cjs` yet? `npx gitnexus analyze` (npm 11 crash → `npm i -g gitnexus`; #1939).

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows. For regression review, compare against the default branch: `detect_changes({scope: "compare", base_ref: "main"})`.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `query({search_query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `context({name: "symbolName"})`.
- For security review, `explain({target: "fileOrSymbol"})` lists taint findings (source→sink flows; needs `analyze --pdg`).

## Never Do

- NEVER edit a function, class, or method without first running `impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `rename` which understands the call graph.
- NEVER commit changes without running `detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/ODM/context` | Codebase overview, check index freshness |
| `gitnexus://repo/ODM/clusters` | All functional areas |
| `gitnexus://repo/ODM/processes` | All execution flows |
| `gitnexus://repo/ODM/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
