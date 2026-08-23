package ratelimit

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	cases := map[string]int64{
		"500":   500,
		"500K":  500 * 1024,
		"5M":    5 * 1024 * 1024,
		"2G":    2 * 1024 * 1024 * 1024,
		"1.5M":  int64(1.5 * 1024 * 1024),
		"5MB/s": 5 * 1024 * 1024,
		"5M/s":  5 * 1024 * 1024,
		"5Kb":   5 * 1024, // lowercase-b suffix: accepted since the single-pass rewrite
	}
	for in, want := range cases {
		got, err := ParseRate(in)
		if err != nil {
			t.Errorf("ParseRate(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRate(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseRateInvalid(t *testing.T) {
	for _, bad := range []string{"", "abc", "5Z"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("want error for %q", bad)
		}
	}
}

func TestUnlimitedNoBlocking(t *testing.T) {
	l, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Unlimited() {
		t.Fatalf("empty spec must be unlimited")
	}
	start := time.Now()
	if err := l.Acquire(context.Background(), 1<<30); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("unlimited acquire must not block, took %v", time.Since(start))
	}
}

// TestReader_Throttles verifies the global bucket actually waits. We set a tiny
// cap (1 KiB/s, burst 1 KiB) and Acquire twice for burst-sized chunks: the
// first consumes the initial burst instantly; the second must wait ~1 second
// for the bucket to refill. Keeps the test deterministic and <2s.
func TestReader_Throttles(t *testing.T) {
	l, err := New("1K")
	if err != nil {
		t.Fatal(err)
	}
	if l.bytes.Load() != 1024 {
		t.Fatalf("rate want 1024 got %d", l.bytes.Load())
	}
	// First Acquire(consumes burst bytes) → near-instant.
	start := time.Now()
	if err := l.Acquire(context.Background(), 1024); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("first acquire should be instant, took %v", time.Since(start))
	}
	// Second Acquire must wait ~1 second to refill.
	start = time.Now()
	if err := l.Acquire(context.Background(), 1024); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 600*time.Millisecond {
		t.Fatalf("not throttled enough: %v (expected ~1s)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("over-throttled: %v", elapsed)
	}

	// And confirm the Reader wrapper honours the same bucket.
	payload := bytes.Repeat([]byte{'x'}, 512) // half of burst after drain
	rr := l.Reader(context.Background(), bytes.NewReader(payload))
	if _, err := io.Copy(io.Discard, rr); err != nil {
		t.Fatalf("reader copy: %v", err)
	}
}

func TestAdaptiveBackOff(t *testing.T) {
	l, err := New("1M")
	if err != nil {
		t.Fatal(err)
	}
	if !l.BackOffSignal() {
		t.Fatal("first BackOffSignal should report a change")
	}
	if got := l.bytes.Load(); got != 512*1024 {
		t.Fatalf("after one halving rate = %d, want %d", got, 512*1024)
	}
	cfg := l.configured.Load()
	if cfg != 1024*1024 {
		t.Fatalf("configured drifted: %d", cfg)
	}
	l.ResetRate()
	if got := l.bytes.Load(); got != 1024*1024 {
		t.Fatalf("ResetRate -> %d, want %d", got, 1024*1024)
	}
	// Repeated halving floors at minAdaptiveBps and stops signalling.
	for i := 0; i < 20; i++ {
		l.BackOffSignal()
	}
	if got := l.bytes.Load(); got != minAdaptiveBps {
		t.Fatalf("rate floor = %d, want %d", got, minAdaptiveBps)
	}

	// Unlimited limiter never signals (nothing sane to halve).
	un, _ := New("")
	if un.BackOffSignal() {
		t.Fatal("unlimited limiter must not signal back-off")
	}

	// An explicit SetRate redefines the restore point.
	l.SetRate("2M")
	l.BackOffSignal()
	l.ResetRate()
	if got := l.bytes.Load(); got != 2*1024*1024 {
		t.Fatalf("restore after SetRate -> %d, want %d", got, 2<<20)
	}
}
