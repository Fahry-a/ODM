// Package ratelimit implements the global token bucket from PRD §11.4.
//
// --limit-rate is enforced by ONE limiter shared across every active worker of
// every task, rather than splitting the rate evenly per connection. Workers
// acquire tokens in proportion to the bytes they are about to Read from the
// network (before writing to disk), so throttling happens at the data-stream
// level and the aggregate throughput stays at the configured ceiling no matter
// how many connections are alive (files finishing, batch queue advancing, …).
//
// When --limit-rate is unset, the limiter is "off" (unlimited) and Acquire is a
// no-op, so there is zero per-byte overhead on the common fast path.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/time/rate"
)

// Limiter is the shared global rate limiter. The underlying *rate.Limiter is
// held in an atomic pointer so SetRate (RPC changeOption) can swap it while
// workers are concurrently calling Acquire/Reader — a plain field would be a
// data race between the RPC goroutine and every active download worker. A nil
// loaded pointer means "unlimited": Acquire/Reader are cheap no-ops.
type Limiter struct {
	lr    atomic.Pointer[rate.Limiter]
	bytes atomic.Int64 // configured rate (bytes/sec), 0 = unlimited
}

// New builds a Limiter from a --limit-rate string ("5M", "500K", "0"/""/off =
// unlimited). Suffixes: B/K/M/G/T, optional trailing 'b'/'B', binary power (K
// = 1024). Empty/unset → unlimited.
func New(spec string) (*Limiter, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "off") || spec == "0" {
		return &Limiter{}, nil
	}
	bps, err := ParseRate(spec)
	if err != nil {
		return nil, err
	}
	if bps <= 0 {
		return &Limiter{}, nil
	}
	// Burst = bps so a burst up to 1s of the cap is allowed (matches x/time/rate
	// ergonomics; the long-run average stays at bps).
	l := &Limiter{}
	l.bytes.Store(bps)
	l.lr.Store(rate.NewLimiter(rate.Limit(bps), int(bps)))
	return l, nil
}

// Unlimited reports whether the limiter is disabled.
func (l *Limiter) Unlimited() bool { return l == nil || l.lr.Load() == nil }

// SetRate updates the global rate limit at runtime. spec is the same format as
// New ("5M", "500K", "off"/""=unlimited). Safe for concurrent use — the limiter
// itself is swapped/stored atomically, and x/time/rate's SetLimit/SetBurst are
// goroutine-safe.
func (l *Limiter) SetRate(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "off") || spec == "0" {
		l.lr.Store(nil)
		l.bytes.Store(0)
		return nil
	}
	bps, err := ParseRate(spec)
	if err != nil {
		return err
	}
	if bps <= 0 {
		l.lr.Store(nil)
		l.bytes.Store(0)
		return nil
	}
	l.bytes.Store(bps)
	cur := l.lr.Load()
	if cur == nil {
		// was unlimited; create a fresh limiter
		l.lr.Store(rate.NewLimiter(rate.Limit(bps), int(bps)))
	} else {
		cur.SetLimit(rate.Limit(bps))
		cur.SetBurst(int(bps))
	}
	return nil
}

// Acquire waits until n bytes worth of tokens are available, honouring ctx
// cancellation. n may be 0 (returns immediately) or negative (no-op).
func (l *Limiter) Acquire(ctx context.Context, n int) error {
	if l.Unlimited() || n <= 0 {
		return nil
	}
	lr := l.lr.Load()
	if err := lr.WaitN(ctx, min(n, lr.Burst())); err != nil {
		return fmt.Errorf("rate: %w", err)
	}
	return nil
}

// Reader wraps r so that reads acquire rate tokens proportional to the number
// of bytes actually returned. It honours ctx for the wait; ctx==nil ⇒
// background context. The returned reader is NOT safe for concurrent use on a
// single reader (one stream → one reader), but the underlying Limiter is shared
// across many concurrent readers — that's the whole point (§11.4).
func (l *Limiter) Reader(ctx context.Context, r io.Reader) *RateReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &RateReader{src: r, l: l, ctx: ctx}
}

// RateReader is an io.Reader that throttles its source against the shared
// Limiter.
//
// Throttling is POST-read: Read first lets the source return up to len(p)
// bytes, then waits for tokens to cover exactly what it got. That means up to
// one burst (by default ≈1s worth of the configured rate, see New) of bytes can
// be in flight at any instant across all concurrent readers sharing the
// Limiter. This is intentional and standard for stream throttling: the token
// wait back-pressures the caller's NEXT read, so the long-run aggregate stays
// at the ceiling while the pipe stays full. A pre-read wait would instead idle
// each connection for a full burst interval on every read, starving throughput
// on small buffers. The in-flight window is bounded by the burst, so the
// worst-case overshoot is one burst — never one burst per read.
//
// A RateReader is NOT safe for concurrent use: create one per stream and use it
// from a single goroutine (the engine wraps each connection's own source). The
// underlying Limiter is shared across many concurrent readers — that sharing is
// what enforces the global ceiling (§11.4).
type RateReader struct {
	src io.Reader
	l   *Limiter
	ctx context.Context
}

// Read implements io.Reader. It delegates the read to src first, then waits
// for tokens matching the bytes received (post-read throttling — see RateReader
// for the burst-in-flight semantics). The *aggregate* across all readers stays
// capped regardless of how buffers are sized per-connection.
func (rr *RateReader) Read(p []byte) (int, error) {
	n, err := rr.src.Read(p)
	if n > 0 && !rr.l.Unlimited() {
		if werr := rr.l.Acquire(rr.ctx, n); werr != nil {
			return n, errors.Join(err, werr)
		}
	}
	return n, err
}

// ParseRate converts a human rate string ("5M", "500K", "2.5G") to bytes/sec.
// Suffix K/M/G/T = powers of 1024; optional trailing 'B' or 'B/s'. Bare
// integer ⇒ bytes/sec.
func ParseRate(spec string) (int64, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return 0, errors.New("empty rate")
	}
	// strip trailing "/s" or "/S" if present
	if strings.HasSuffix(s, "/s") || strings.HasSuffix(s, "/S") {
		s = strings.TrimSpace(s[:len(s)-2])
	}
	mult := int64(1)
	trim := 0
	u := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(u, "KB"):
		mult, trim = 1024, 2
	case strings.HasSuffix(u, "K"):
		mult, trim = 1024, 1
	case strings.HasSuffix(u, "MB"):
		mult, trim = 1024*1024, 2
	case strings.HasSuffix(u, "M"):
		mult, trim = 1024*1024, 1
	case strings.HasSuffix(u, "GB"):
		mult, trim = 1024*1024*1024, 2
	case strings.HasSuffix(u, "G"):
		mult, trim = 1024*1024*1024, 1
	case strings.HasSuffix(u, "TB"):
		mult, trim = 1024*1024*1024*1024, 2
	case strings.HasSuffix(u, "T"):
		mult, trim = 1024*1024*1024*1024, 1
	}
	if trim > 0 {
		s = s[:len(s)-trim]
	}
	s = strings.TrimSpace(s)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q: %w", spec, err)
	}
	// Multiply in float then truncate so fractional rates keep precision
	// (int64(1.5)*mult would lose the .5).
	return int64(v * float64(mult)), nil
}
