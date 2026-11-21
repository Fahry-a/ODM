# golang.org/x/time/rate — semantics for a Rust port

Source: golang/time/rate/rate.go (master); ODM: internal/ratelimit/bucket.go.

- **Types**: `Limit` = `f64` (events/sec); `Inf = Limit(math.MaxFloat64)`; `Every(interval) = 1/interval.Seconds()` (or `Inf` if interval <= 0).
- **`NewLimiter(r, b)`**: bucket of size `b`, initially full (`tokens = b`), refilled at `r`/sec (ignored when `r == Inf`).
- **`Burst()`** — max tokens consumable per single call; ODM reads it in `Acquire`. Also `Limit()`, `Tokens()`, `TokensAt(t)` (non-mutating).
- **Blocking `WaitN(ctx, n)`** — reserve n tokens, block until available/cancel. Errors: `n > burst` (when r != Inf) → `"rate: Wait(n=N) exceeds limiter's burst B"`; ctx done → `ctx.Err()`; wait past ctx deadline → `"rate: Wait(n=N) would exceed context deadline"`; on cancel the reservation is reversed.
- **Non-blocking**: `AllowN(t, n) bool` = `reserveN(t, n, 0).ok`; `ReserveN(t, n) *Reservation` with `OK()`/`DelayFrom(t)`.
- **ODM usage**: one shared `rate.Limiter` via `atomic.Pointer` across all workers. `rate.NewLimiter(rate.Limit(bps), int(bps))` — rate = bps bytes/sec, burst = bps B (starts full). `Acquire(ctx, n)` = `lr.WaitN(ctx, min(n, lr.Burst()))` (caps at burst, so it never hits the n>burst error; errors wrapped `"rate: %w"`). `setBytes` = `SetLimit` + `SetBurst`.
- **Minimal O(1) Rust design**:

```rust
pub struct TokenBucket { rate_bps: f64, burst: f64, tokens: f64, last: Instant }

impl TokenBucket {
    fn refill(&mut self) {
        let now = Instant::now();
        self.tokens = (self.tokens + now.duration_since(self.last).as_secs_f64() * self.rate_bps).min(self.burst);
        self.last = now;
    }
    pub fn try_acquire(&mut self, n: f64) -> bool {
        self.refill();
        if self.tokens >= n { self.tokens -= n; true } else { false }
    }
    pub fn wait_for(&mut self, n: f64) -> Duration {
        self.refill();
        Duration::from_secs_f64((n - self.tokens).max(0.0) / self.rate_bps)
    }
}
// async acquire: loop { if try_acquire(n) { return } tokio::time::sleep(wait_for(n)).await }
```
