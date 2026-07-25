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

	"golang.org/x/time/rate"
)

// Limiter is the shared global rate limiter. A nil Limiter or one with lr==nil
// means "unlimited": Acquire/Reader are cheap no-ops.
type Limiter struct {
	lr    *rate.Limiter // nil ⇒ unlimited
	bytes int64         // configured rate (bytes/sec), 0 = unlimited
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
	return &Limiter{lr: rate.NewLimiter(rate.Limit(bps), int(bps)), bytes: bps}, nil
}

// BytesPerSec returns the configured rate, or 0 when unlimited.
func (l *Limiter) BytesPerSec() int64 { return l.bytes }

// Unlimited reports whether the limiter is disabled.
func (l *Limiter) Unlimited() bool { return l == nil || l.lr == nil }

// Acquire waits until n bytes worth of tokens are available, honouring ctx
// cancellation. n may be 0 (returns immediately) or negative (no-op).
func (l *Limiter) Acquire(ctx context.Context, n int) error {
	if l.Unlimited() || n <= 0 {
		return nil
	}
	if err := l.lr.WaitN(ctx, min(n, l.lr.Burst())); err != nil {
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
// Limiter. Read pulls tokens for the bytes it is about to read; we ask the
// source for up to p bytes and pay for exactly what it returns.
type RateReader struct {
	src io.Reader
	l   *Limiter
	ctx context.Context
}

// Read implements io.Reader. We delegate the read to src and then wait for
// tokens matching the bytes received, so the *aggregate* across all readers
// stays capped regardless of how buffers are sized per-connection.
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
		s = s[:len(s)-2]
	}
	s = strings.TrimSpace(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k") || strings.HasSuffix(s, "KB") || strings.HasSuffix(s, "kb"):
		mult = 1024
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m") || strings.HasSuffix(s, "MB") || strings.HasSuffix(s, "mb"):
		mult = 1024 * 1024
	case strings.HasSuffix(s, "G") || strings.HasSuffix(s, "g") || strings.HasSuffix(s, "GB") || strings.HasSuffix(s, "gb"):
		mult = 1024 * 1024 * 1024
	case strings.HasSuffix(s, "T") || strings.HasSuffix(s, "t") || strings.HasSuffix(s, "TB") || strings.HasSuffix(s, "tb"):
		mult = 1024 * 1024 * 1024 * 1024
	}
	// remove the suffix chars from s for parsing the numeric part
	trim := []string{"KB", "kb", "K", "k", "MB", "mb", "M", "m", "GB", "gb", "G", "g", "TB", "tb", "T", "t"}
	for _, t := range trim {
		if strings.HasSuffix(s, t) {
			s = s[:len(s)-len(t)]
			break
		}
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
