// Package ratelimit implements the global token bucket limiter
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
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

type Limiter struct {
	lr    atomic.Pointer[rate.Limiter]
	bytes atomic.Int64

	configured atomic.Int64
	// cooldownUntil is shared by every task/worker using this limiter. A 429
	// from one task therefore cannot be immediately undone by a successful
	// chunk in another task.
	cooldownUntil atomic.Int64 // unix nanos; 0 = no adaptive cooldown
}

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
	if bps > int64(maxInt()) {
		return nil, fmt.Errorf("rate %d exceeds platform burst capacity", bps)
	}
	l := &Limiter{}
	l.bytes.Store(bps)
	l.configured.Store(bps)
	l.lr.Store(rate.NewLimiter(rate.Limit(bps), int(bps)))
	return l, nil
}

func (l *Limiter) Unlimited() bool { return l == nil || l.lr.Load() == nil }

const minAdaptiveBps = 64 * 1024
const adaptiveCooldown = 30 * time.Second

func (l *Limiter) BackOffSignal() bool {
	if l == nil {
		return false
	}
	cur := l.bytes.Load()
	if cur <= 0 {
		return false
	}
	half := cur / 2
	if half < minAdaptiveBps {
		half = minAdaptiveBps
	}
	if half == cur {
		return false
	}
	l.setBytes(half)
	l.cooldownUntil.Store(time.Now().Add(adaptiveCooldown).UnixNano())
	return true
}

// ThrottleOK restores the configured rate only after the GLOBAL cooldown has
// expired. It is safe to call from any task/worker sharing the limiter.
func (l *Limiter) ThrottleOK() {
	if l == nil {
		return
	}
	until := l.cooldownUntil.Load()
	if until == 0 || time.Now().UnixNano() < until {
		return
	}
	if l.cooldownUntil.CompareAndSwap(until, 0) {
		l.ResetRate()
	}
}

func (l *Limiter) ResetRate() {
	if l == nil {
		return
	}
	if cfg := l.configured.Load(); cfg > 0 && l.bytes.Load() != cfg {
		l.setBytes(cfg)
	}
}

func (l *Limiter) setBytes(bps int64) {
	l.bytes.Store(bps)
	cur := l.lr.Load()
	burst := int(bps)
	if cur == nil {
		l.lr.Store(rate.NewLimiter(rate.Limit(bps), burst))
	} else {
		cur.SetLimit(rate.Limit(bps))
		cur.SetBurst(burst)
	}
}

func (l *Limiter) SetRate(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "off") || spec == "0" {
		l.lr.Store(nil)
		l.bytes.Store(0)
		l.cooldownUntil.Store(0)
		return nil
	}
	bps, err := ParseRate(spec)
	if err != nil {
		return err
	}
	if bps <= 0 {
		l.lr.Store(nil)
		l.bytes.Store(0)
		l.cooldownUntil.Store(0)
		return nil
	}
	if bps > int64(maxInt()) {
		return fmt.Errorf("rate %d exceeds platform burst capacity", bps)
	}
	l.bytes.Store(bps)
	l.configured.Store(bps)
	l.cooldownUntil.Store(0)
	cur := l.lr.Load()
	if cur == nil {
		l.lr.Store(rate.NewLimiter(rate.Limit(bps), int(bps)))
	} else {
		cur.SetLimit(rate.Limit(bps))
		cur.SetBurst(int(bps))
	}
	return nil
}

func (l *Limiter) Acquire(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	lr := l.lr.Load()
	if lr == nil {
		return nil
	}
	if err := lr.WaitN(ctx, min(n, lr.Burst())); err != nil {
		return fmt.Errorf("rate: %w", err)
	}
	return nil
}

func (l *Limiter) Reader(ctx context.Context, r io.Reader) *RateReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &RateReader{src: r, l: l, ctx: ctx}
}

type RateReader struct {
	src io.Reader
	l   *Limiter
	ctx context.Context
}

func (rr *RateReader) Read(p []byte) (int, error) {
	n, err := rr.src.Read(p)
	if n > 0 {
		if werr := rr.l.Acquire(rr.ctx, n); werr != nil {
			return n, errors.Join(err, werr)
		}
	}
	return n, err
}

func ParseRate(spec string) (int64, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return 0, errors.New("empty rate")
	}
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
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("invalid rate %q", spec)
	}
	max := float64(maxInt64()) / float64(mult)
	if v > max {
		return 0, fmt.Errorf("rate %q overflows int64", spec)
	}
	bps := int64(v * float64(mult))
	if bps > int64(maxInt()) {
		return 0, fmt.Errorf("rate %q exceeds platform int capacity", spec)
	}
	return bps, nil
}

func maxInt() int { return int(^uint(0) >> 1) }
func maxInt64() int64 { return int64(^uint64(0) >> 1) }
