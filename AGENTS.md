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
internal/config → CLI flags (pflag) + config file merge; owns Version const
internal/download → Task lifecycle, chunk queue (work-stealing), Manager, exit codes, 2nd Version const
internal/transport → HTTP client, Probe (HEAD → ranged GET → single-stream fallback)
internal/scheduler → Connection Balancer (allocation modes A/B/C), Daemon (RPC), Queue
internal/ui → Pacman/ILoveCandy progress bar, confirmation prompt, terminal sizing
internal/rpc → JSON-RPC 2.0 + WebSocket server, Broadcaster
internal/ratelimit → Global token bucket (--limit-rate)
internal/storage → File WriteAt, resume control files (.odm)
internal/logging → Leveled logger (--log / --log-level)
```

## Key conventions

- **Module path is `odm`** (not a GitHub URL). Imports look like `odm/internal/config`. Keep it this way.
- **Version is duplicated** in `internal/config/config.go` and `internal/download/manager.go` — both must match when bumping. This is intentional to avoid an import cycle (see comment at `manager.go:187-189`). Update `packaging/PKGBUILD` `pkgver` too.
- **Progress bar format**: `internal/ui/render.go` is the pure rendering layer (`Bar()`, `BarIndeterminate()`, `renderTaskLine()`). `internal/ui/progress.go` owns the stateful `Renderer` (cache, animation tick, TTY redraw loop). Keep `render.go` functions pure/testable — animation state lives in `progress.go`.
- **Bar width is fixed** at 30 cells (`BarWidth` const). Dots are spaced (`o o o`), face animates `c`↔`C` every ~1s. `barLine()` pads to exactly `width` so the `%` column never shifts. `TestRenderTaskLine_FixedColumns` pins this.
- **ETA format is `HH:MM:SS`** (8 cells, `colETA=8`). `FormatDuration` caps at `99:59:59`. The download engine's `estimateETA` (`task.go`) must NOT multiply by `time.Second` a second time — the inner expression already yields nanoseconds.
- **`Snapshot()` is the read API** for task state. UI and RPC both read `download.ProgressView` via `Task.Snapshot()`. Write path is atomic fields (`conns`, `speed`, `bytesDone`, `state`).
- **Exit codes**: `0`=ok, `1`=args, `2`=network, `3`=partial, `4`=cancelled (`download.Exit*` constants).

## Testing quirks

- `internal/rpc` tests start real httptest servers with real downloads; they take several seconds under `-race`. `TestServer_AddURIAndTellActive` was previously documented as flaky but passes consistently in recent runs (the test uses a 2 s tolerant poll loop + checks both `tellActive` and `tellWaiting`).
- `internal/download` tests use httptest servers with real chunk downloads; they take several seconds under `-race`.
- `internal/storage` tests use `t.TempDir()` and concurrent goroutines to stress `WriteAt` non-overlap.
- `internal/logging` tests swap the unexported `from`/`file` fields to capture output in a buffer (same-package test access).
- UI tests inject a fake clock (`nowFn` in `clock.go`) — restore `nowFn` in `t.Cleanup` if you change it.

## Cross-compiling

```bash
export CGO_ENABLED=0 GOFLAGS="-trimpath -mod=readonly" GOTOOLCHAIN=local
LDFLAGS="-s -w -buildid="
GOOS=linux GOARCH=amd64 go build -ldflags="$LDFLAGS" -o build/odm_0.1.0_linux_amd64 ./cmd/odm
# also: 386, arm, arm64, darwin/amd64, darwin/arm64
```

`build/` is gitignored — output cross-compile binaries there.

## Releases

Automated via `.github/workflows/release.yml`. Push a `v*` tag → workflow verifies versions, cross-compiles, and creates a GitHub Release.

**Release checklist (all 3 must match the tag):**

1. `internal/config/config.go:26` — `const Version = "odm/X.Y.Z"`
2. `internal/download/manager.go:190` — `const Version = "odm/X.Y.Z"`
3. `packaging/PKGBUILD:10` — `pkgver=X.Y.Z`

**Release steps:**

```bash
# 1. Bump versions in all 3 locations above
# 2. Move [Unreleased] section in CHANGELOG.md → new version header
# 3. Commit
git add -A && git commit -m "release: vX.Y.Z"

# 4. Tag & push
git tag vX.Y.Z
git push origin main && git push origin vX.Y.Z
```

**What the workflow does:**

1. Extracts version from git tag (strips `v` prefix)
2. Extracts versions from `config.go`, `manager.go`, `PKGBUILD` — fails if any mismatch
3. Cross-compiles 6 targets: `linux/{386,amd64,arm,arm64}`, `darwin/{amd64,arm64}`
4. Parses `CHANGELOG.md` for the version section (Keep a Changelog format)
5. Generates SHA-256 checksums
6. Creates GitHub Release via `softprops/action-gh-release@v2`
   - Pre-release auto-detected if version contains `-` (e.g. `v0.2.0-rc1`)

**Binary naming:** `odm_X.Y.Z_<os>_<arch>` (e.g. `odm_0.2.0_linux_amd64`)

**Manual fallback (if needed):**

```bash
gh release create vX.Y.Z --prerelease build/odm_X.Y.Z_* --title "vX.Y.Z" --notes "..."
```

Tag format: `v<semver>`. Pre-releases use `--prerelease`.
