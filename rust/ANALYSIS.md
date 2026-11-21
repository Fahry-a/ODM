# Rust migration plan: ODM (Go)

1 packages, 42 source files (9247 lines), 37 test files, 0 UI files, grouped into 12 units.

| Ready | Caution | Blocked | Keep in source language |
| ---: | ---: | ---: | ---: |
| 5 | 7 | 0 | 0 |

## Start here

Best first ports: safe to port (readiness) and worth porting (loops and string work, tests, size).

| # | Unit | Lines | Priority | Readiness | Tests | Bridge | Main risks |
| ---: | --- | ---: | ---: | ---: | ---: | --- | --- |
| 1 | `internal/storage/` | 198 | 66 | 77 | 3 | cgo | Pointers and manual memory (13); Null references (23); JSON round-trips (1) |
| 2 | `internal/update/` | 280 | 61 | 75 | 0 | cgo | Pointers and manual memory (1); Null references (22); Integer width and overflow (2) |
| 3 | `internal/transport/` | 489 | 53 | 51 | 12 | cgo | Pointers and manual memory (34); Null references (58); Callback-shaped APIs (2) |
| 4 | `internal/logging/` | 134 | 51 | 68 | 0 | cgo | Pointers and manual memory (13); Null references (14); Callback-shaped APIs (3) |

### `internal/storage/`

198 lines in 2 files, wave 0, priority 66, readiness 77, bridge **cgo**.

- Leaf: imports no other unit.
- 3 test files import it directly.
- Pointers and manual memory x13: `internal/storage/file.go:20`, `internal/storage/file.go:27`, `internal/storage/file.go:50`
- Null references x23: `internal/storage/file.go:29`, `internal/storage/file.go:34`, `internal/storage/file.go:35`
- JSON round-trips x1: `internal/storage/resume.go:4`

### `internal/update/`

280 lines in 1 files, wave 0, priority 61, readiness 75, bridge **cgo**.

- Leaf: imports no other unit.
- Pointers and manual memory x1: `internal/update/update.go:40`
- Null references x22: `internal/update/update.go:34`, `internal/update/update.go:40`, `internal/update/update.go:49`
- Integer width and overflow x2: `internal/update/update.go:270`, `internal/update/update.go:290`
- Callback-shaped APIs x1: `internal/update/update.go:230`
- Exceptions x1: `internal/update/update.go:230`
- JSON round-trips x1: `internal/update/update.go:7`

### `internal/transport/`

489 lines in 1 files, wave 0, priority 53, readiness 51, bridge **cgo**.

- 1 Go modules dependencies not in the crate table: check them by hand.
- Leaf: imports no other unit.
- 12 test files import it directly.
- Pointers and manual memory x34: `internal/transport/transport.go:54`, `internal/transport/transport.go:65`, `internal/transport/transport.go:73`
- Null references x58: `internal/transport/transport.go:117`, `internal/transport/transport.go:118`, `internal/transport/transport.go:130`
- Callback-shaped APIs x2: `internal/transport/transport.go:113`, `internal/transport/transport.go:141`
- Dynamic typing x1: `internal/transport/transport.go:223`
- Exceptions x2: `internal/transport/transport.go:205`, `internal/transport/transport.go:422`

### `internal/logging/`

134 lines in 1 files, wave 0, priority 51, readiness 68, bridge **cgo**.

- Leaf: imports no other unit.
- Pointers and manual memory x13: `internal/logging/logging.go:60`, `internal/logging/logging.go:61`, `internal/logging/logging.go:62`
- Null references x14: `internal/logging/logging.go:29`, `internal/logging/logging.go:31`, `internal/logging/logging.go:33`
- Callback-shaped APIs x3: `internal/logging/logging.go:117`, `internal/logging/logging.go:119`, `internal/logging/logging.go:121`
- Concurrency x1: `internal/logging/logging.go:12`
- Exceptions x2: `internal/logging/logging.go:90`, `internal/logging/logging.go:102`

## Order by package

Packages come after the workspace packages they use. Inside a package, port by wave: a unit's imports are in earlier waves.

### 1. odm (`.`, portable, 37 test files)

| Wave | Unit | Status | Lines | Readiness | Bridge | Main risks |
| ---: | --- | --- | ---: | ---: | --- | --- |
| 0 | `internal/version/` | ready | 7 | 98 | cgo | - |
| 0 | `internal/storage/` | ready | 198 | 77 | cgo | Pointers and manual memory (13); Null references (23) |
| 0 | `internal/update/` | ready | 280 | 75 | cgo | Pointers and manual memory (1); Null references (22) |
| 0 | `internal/logging/` | ready | 134 | 68 | cgo | Pointers and manual memory (13); Null references (14) |
| 0 | `internal/transport/` | ready | 489 | 51 | cgo | Pointers and manual memory (34); Null references (58) |
| 0 | `internal/ratelimit/` | caution | 227 | 49 | cgo | Pointers and manual memory (16); String offsets and slicing (2) |
| 1 | `internal/config/` | caution | 780 | 34 | cgo | Pointers and manual memory (83); String offsets and slicing (3) |
| 1 | `internal/download/` | caution | 2786 | 0 | cgo | Pointers and manual memory (148); String offsets and slicing (11) |
| 2 | `internal/scheduler/` | caution | 961 | 10 | cgo | Pointers and manual memory (71); String offsets and slicing (2) |
| 3 | `internal/rpc/` | caution | 658 | 5 | cgo | Pointers and manual memory (106); String offsets and slicing (2) |
| 3 | `internal/ui/` | caution | 1843 | 0 | cgo | Pointers and manual memory (20); String offsets and slicing (9) |
| 4 | `cmd/odm/` | caution | 884 | 15 | cgo | Pointers and manual memory (24); String offsets and slicing (3) |

## Go modules dependencies

| Package | Units | Rust | Fit | Note |
| --- | ---: | --- | --- | --- |
| `github.com/spf13/pflag` | 2 | ? | unknown |  |
| `github.com/gorilla/websocket` | 1 | ? | unknown |  |
| `golang.org/x/net/proxy` | 1 | ? | unknown |  |
| `golang.org/x/time/rate` | 1 | ? | unknown |  |

## Risk guide

- **String offsets and slicing** (High): JS indexes strings in UTF-16 code units, Rust in UTF-8 bytes. Offsets that cross the boundary must be converted, or non-ASCII text gets wrong ranges silently.
- **Dynamic typing** (Medium): Values whose shape is checked at runtime need an explicit Rust enum or `serde_json::Value`.
- **Exceptions** (Low): Thrown errors become `Result`. Errors crossing the bridge must map back to the same JS error shape.
- **Callback-shaped APIs** (Medium): Function-typed parameters and methods cross the bridge on every call. Keep boundaries coarse: pass data in, get data out.
- **JSON round-trips** (Low): JSON.stringify key order and number formatting must match `serde_json` output if the result is user-visible.
- **Integer width and overflow** (Medium): Python ints never overflow; C, Go and Java ints wrap or are undefined on overflow. Pick i64, i128 or a bignum crate deliberately, and use wrapping_* where the source wraps.
- **Division and modulo** (Medium): Python `//` and `%` floor toward negative infinity; Rust `/` and `%` truncate: use `div_euclid`/`rem_euclid` or explicit floor math. C division by zero is undefined.
- **Pointers and manual memory** (High): Pointer arithmetic and manual memory become slices, indices and owned types. Code that exposes pointers across the boundary needs `unsafe`, isolated and explained.
- **Concurrency** (Medium): Threads, goroutines and locks need a design in Rust (channels, Send/Sync), not a line-by-line translation; keep concurrency in the caller where possible.
- **Null references** (Medium): Any reference may be null in the source: decide per parameter whether it is `Option` and what null means, instead of mapping every value to `Option`.

## Whole app

Shape: **server**.

| Entry point | Kind | Found from |
| --- | --- | --- |
| odm: `cmd/odm` | cli | a `cmd/` folder |
| `cmd/odm/main.go` | server | `ListenAndServe` at cmd/odm/main.go:795 |

### Every dependency

| Dependency | Declared | Used by | Rust | Fit | Note |
| --- | --- | --- | --- | --- | --- |
| `github.com/spf13/pflag` | yes | 2 | ? | unknown |  |
| `github.com/gorilla/websocket` | yes | 1 | ? | unknown |  |
| `golang.org/x/net/proxy` | no | 1 | ? | unknown |  |
| `golang.org/x/time/rate` | no | 1 | ? | unknown |  |
| `golang.org/x/net` | yes | 0 | ? | unknown |  |
| `golang.org/x/time` | yes | 0 | ? | unknown |  |

### Pieces, in port order

- wave 0: `internal/logging/` (134 lines)
- wave 0: `internal/ratelimit/` (227 lines)
- wave 0: `internal/storage/` (198 lines)
- wave 0: `internal/transport/` (489 lines)
- wave 0: `internal/update/` (280 lines)
- wave 0: `internal/version/` (7 lines)
- wave 1: `internal/config/` (780 lines)
- wave 1: `internal/download/` (2786 lines)
- wave 2: `internal/scheduler/` (961 lines)
- wave 3: `internal/rpc/` (658 lines)
- wave 3: `internal/ui/` (1843 lines)
- wave 4: `cmd/odm/` (884 lines)

### Decisions for the user

**D1** (shape): The app is a server, entered through odm (cmd/odm), cmd/odm/main.go. Port every entry point, in that shape?

> Yes: an axum front door (`app front-door`) takes the server over first

**D2** (dependency github.com/spf13/pflag): Which crate replaces github.com/spf13/pflag (used by 2 units)?

> Research it against primary sources into rust/research/github.com-spf13-pflag.md before porting what uses it

**D3** (dependency github.com/gorilla/websocket): Which crate replaces github.com/gorilla/websocket (used by 1 unit)?

> Research it against primary sources into rust/research/github.com-gorilla-websocket.md before porting what uses it

**D4** (dependency golang.org/x/net/proxy): Which crate replaces golang.org/x/net/proxy (used by 1 unit)?

> Research it against primary sources into rust/research/golang.org-x-net-proxy.md before porting what uses it

**D5** (dependency golang.org/x/time/rate): Which crate replaces golang.org/x/time/rate (used by 1 unit)?

> Research it against primary sources into rust/research/golang.org-x-time-rate.md before porting what uses it

**D6** (dependency golang.org/x/net): Which crate replaces golang.org/x/net (declared, no unit imports it)?

> Research it against primary sources into rust/research/golang.org-x-net.md before porting what uses it

**D7** (dependency golang.org/x/time): Which crate replaces golang.org/x/time (declared, no unit imports it)?

> Research it against primary sources into rust/research/golang.org-x-time.md before porting what uses it

