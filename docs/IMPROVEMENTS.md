# ODM — Improvement Notes

> Review notes based on the current `main` branch. These are engineering priorities and observations, not claims that every item is currently broken.

## Priority overview

| Priority | Area | Recommendation |
|---|---|---|
| 🔴 High | Scheduler / Balancer | Add property-based and invariant tests for connection allocation |
| 🔴 High | Benchmarking | Benchmark ODM against aria2c and benchmark each engine/profile |
| 🔴 High | Reliability | Add fault-injection/stress tests for retries, cancellation, resume, and partial failures |
| 🟠 Medium | `smart` profile | Replace static heuristics with measurements from probing/telemetry where practical |
| 🟠 Medium | `both` profile | Validate the complexity with real benchmarks before expanding the feature |
| 🟠 Medium | `main.go` | Keep the CLI entry point thin; move orchestration into application-level components if it keeps growing |
| 🟡 Low | Documentation | Document scheduler invariants and engine-selection rationale |

---

## 1. Connection Balancer testing

### Why

`internal/scheduler/balancer.go` is deliberately designed as a pure function, which makes it an excellent target for exhaustive/property-based testing.

The current logic has several interacting rules:

- single-file mode (`Mode A`)
- batch mode (`Mode B`)
- explicit split-file mode (`Mode C`)
- remainder distribution
- non-Range fallback
- reallocation of freed connections
- maximum-connection warnings

### Recommended invariants

For valid inputs, test properties such as:

```text
allocated connections never exceed the usable connection budget
non-Range files always receive exactly one connection
single-file mode never produces a queued task
queued tasks receive a valid allocation when admitted
connections are never zero or negative
all URLs appear exactly once in Parallel + Queued
```

Also test edge cases:

```text
-c 1
-c == -sf
-c < -sf
-sf 1
-sf == max-connections
more files than the connection budget
all files without Range support
some files without Range support
remainder smaller/equal/larger than number of parallel files
-c above --max-connections
```

### Suggested implementation

Use normal table-driven tests for documented examples, then add property-based tests/fuzzing for combinations of `C`, `SF`, file count, and Range support.

---

## 2. Benchmark the actual download performance

ODM has several interesting strategies, but performance should be measured rather than inferred from architecture alone.

Create a reproducible benchmark matrix comparing:

- ODM `odm` profile
- ODM `aria2c` profile
- ODM `both` profile
- ODM `smart` profile
- aria2c
- optionally a single-stream baseline

Measure at least:

- total download time
- average and peak throughput
- connection count
- CPU usage
- memory usage
- number of retries
- bytes downloaded/re-requested
- behavior at different RTTs
- behavior with HTTP/1.1 vs HTTP/2
- behavior with and without Range support

Test multiple file sizes and chunk sizes. Avoid optimizing based on one server or one LAN test.

---

## 3. Stress and fault-injection testing

The downloader has several failure-sensitive components: network requests, chunk state, resume, retries, mirrors, and cancellation.

Add tests that intentionally simulate:

- connection reset
- timeout
- HTTP 429
- HTTP 5xx
- malformed/incorrect `Content-Range`
- server changing its ETag
- checksum mismatch
- mirror becoming unavailable
- one worker becoming much slower than others
- cancellation while workers are downloading
- process interruption followed by resume
- disk/write failure

The goal is to prove that a failed worker cannot corrupt the final file or leave the scheduler in an inconsistent state.

---

## 4. Review the `both` engine before adding more complexity

The `both` profile combines ODM's HTTP/1.1 chunk engine with the HTTP/2-oriented engine over different regions of a file.

This is technically interesting, but it increases complexity around:

- retry behavior
- cancellation
- region boundaries
- throughput imbalance
- failure recovery
- server-side throttling
- accounting and progress reporting

Keep it if benchmarks demonstrate a meaningful advantage. Otherwise, consider making `smart` prefer a simpler engine in more cases.

---

## 5. Make `smart` data-driven over time

Static rules such as file-size and connection-count thresholds are useful as a starting point, but they are only heuristics.

The best engine can depend on:

- RTT
- server response time
- Range support
- HTTP protocol
- observed throughput
- connection establishment cost
- server throttling
- file size

A future approach could use the existing probe to collect cheap signals, then select the least complex engine likely to perform well.

Do not make this unnecessarily complicated until benchmark data shows that the current rules are insufficient.

---

## 6. Keep `main.go` as a composition root

`cmd/odm/main.go` is already responsible for connecting many subsystems. As features grow, avoid turning it into a central implementation file.

Prefer a shape like:

```text
cmd/odm/main.go
    -> app.Run()
        -> config
        -> probe
        -> scheduler
        -> download manager
        -> UI
        -> RPC
```

The command package should primarily parse/assemble dependencies and start the application. Domain logic should remain in `internal/*` packages.

---

## 7. Document scheduler invariants

The Connection Balancer is ODM's main differentiator. Its behavior should be documented as a small formal contract, not only through examples.

For example:

```text
C = global connection budget
SF = requested per-file connections in Mode C
R = number of Range-capable files

non-Range file => 1 connection
Mode A => one file consumes the usable budget
Mode B => one connection per running file
Mode C => SF baseline per running file + remainder distribution
```

This makes future scheduler refactors safer and makes tests easier to understand.

---

## 8. Add race/concurrency validation to the normal development workflow

Because ODM is heavily goroutine-based, routinely run:

```bash
go test ./...
go test -race ./...
go vet ./...
```

and keep concurrency-heavy tests deterministic where possible.

Also consider fuzzing parsers and state-transition code that consumes network/server data.

---

## 9. Avoid feature growth before proving the core

ODM already has a broad feature set: multiple engines, resume, mirrors, Metalink, rate limiting, RPC/WebSocket, progress UI, and packaging.

The next stage should emphasize:

1. correctness
2. reproducible benchmarks
3. reliability under failure
4. API stability
5. performance profiling

Only then should additional features be prioritized.

---

## Suggested roadmap

### Phase 1 — Correctness

- [ ] Expand scheduler table tests
- [ ] Add balancer invariants/property tests
- [ ] Add `go test -race ./...` to CI
- [ ] Add fault-injection tests
- [ ] Test resume/corruption/ETag scenarios

### Phase 2 — Performance

- [ ] Build a reproducible local HTTP/HTTPS benchmark server
- [ ] Compare ODM profiles against aria2c
- [ ] Test different `-c` values
- [ ] Test different chunk sizes
- [ ] Test HTTP/1.1 vs HTTP/2
- [ ] Profile CPU and memory

### Phase 3 — Adaptive behavior

- [ ] Analyze benchmark results
- [ ] Tune `smart` thresholds from measured data
- [ ] Re-evaluate whether `both` consistently provides a benefit
- [ ] Consider lightweight runtime adaptation only if justified by data

### Phase 4 — Maintainability

- [ ] Keep `cmd/odm/main.go` thin
- [ ] Document scheduler contracts
- [ ] Document engine-selection decisions
- [ ] Keep RPC/API behavior covered by tests

---

## Bottom line

ODM's architecture is already strong enough that the biggest opportunity is **not adding more features**. The project should now prove its claims through tests, fault injection, and reproducible benchmarks.

The most important question for the next stage is:

> **Does ODM's scheduler and multi-engine architecture produce a measurable advantage over simpler download strategies under realistic network conditions?**

Answering that with data will make the project substantially stronger than adding another feature without measurement.
