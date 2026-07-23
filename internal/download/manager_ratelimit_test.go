package download

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLimitRate_StableAggregate is the PRD §16 rate-limiting acceptance test:
// --limit-rate is a GLOBAL token bucket shared across all workers of all tasks
// (§11.4), so the aggregate throughput stays near the configured cap regardless
// of how many connections are active. A naive per-connection split would make
// -c 8 hit ~8× the cap; here both -c 1 and -c 8 must land close to the same cap.
//
// We run the real engine against a fast in-memory server (no server-side
// throttling) at a modest cap and measure bytes/wall-clock for each connection
// count. Both runs must (a) finish within a generous tolerance band of the cap,
// not blow past it by multiplicative factors, and (b) be close to each other —
// proving the cap is aggregate, not per-connection.
func TestLimitRate_StableAggregate(t *testing.T) {
	const chunk = 4 * 1024
	const numChunks = 1024   // 4 MiB payload — small enough to keep the test fast, large enough that the limiter's ~1s initial burst amortizes into near-steady-state
	const capStr = "1M"      // 1 MiB/s global cap
	capBps := int64(1 * 1024 * 1024)

	// The global bucket's burst == 1s of cap (1 MiB here), so a 4 MiB transfer
	// runs ~3 s of steady-state throttling after a 1 MiB instant burst. The
	// measured throughput converges to cap * r/(r-1) where r = payload/burst;
	// at r=4 that's ~1.33× cap. The window below brackets that with room for
	// scheduling/disk jitter while still catching the failure it guards against
	// (an 8-connection run blowing ~8× past the cap, or starving far below it).
	lo := 0.40 // at least ~40% of cap (warmup + jitter headroom)
	hi := 1.90 // at most ~190% of cap (steady-state ~1.33× + the 1s burst + jitter)

	payload := make([]byte, int64(chunk)*numChunks)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := serveRangeServerRL(t, payload)
	defer srv.Close()

	// measured runs the engine at the given connection count and returns the
	// ratio of achieved throughput to the configured cap (must be in [lo, hi]).
	measured := func(t *testing.T, conns int) float64 {
		t.Helper()
		dir := t.TempDir()
		m, err := NewManager(ExecOptions{
			Dir:         dir,
			OutFile:     "rate.bin",
			Connections: conns,
			Retry:       0,
			RetryWait:   0,
			Continue:    false,
			ChunkSize:   chunk,
			Timeout:     30 * time.Second,
			MaxRedirect: 5,
			CheckCert:   true,
			LimitRate:   capStr,
		}, nil)
		if err != nil {
			t.Fatalf("NewManager(c=%d): %v", conns, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		start := time.Now()
		if err := m.Run(ctx, srv.URL, conns); err != nil {
			t.Fatalf("download(c=%d): %v", conns, err)
		}
		elapsed := time.Since(start)

		got, err := os.ReadFile(filepath.Join(dir, "rate.bin"))
		if err != nil {
			t.Fatalf("read(c=%d): %v", conns, err)
		}
		if len(got) != len(payload) {
			t.Fatalf("size(c=%d): want %d got %d", conns, len(payload), len(got))
		}

		secs := elapsed.Seconds()
		if secs <= 0 {
			t.Fatalf("elapsed non-positive: %v", elapsed)
		}
		achievedBps := float64(len(got)) / secs
		// The cap is respected at the data-stream level (bytes read). We compare
		// to capBps, NOT capBps/conns — that is the whole point of the global
		// bucket.
		return achievedBps / float64(capBps)
	}

	// Sanity: single-connection baseline should already be near the cap. If the
	// server/disk is the bottleneck (achieved ≪ cap), the test is vacuous —
	// fail loudly so we don't pass a rate limiter that isn't actually engaging.
	r1 := measured(t, 1)
	t.Logf("ratio: c=1 -> %.2f (cap %s)", r1, capStr)
	if r1 < lo {
		t.Fatalf("c=1 only achieved %.2f× of cap (<%.2f) — server/disk too slow for the cap to bind; test vacuous", r1, lo)
	}

	r8 := measured(t, 8)
	t.Logf("ratio: c=8 -> %.2f (cap %s)", r8, capStr)

	// (a) Both runs land near the cap, neither starves nor blows through it.
	for c, r := range map[int]float64{1: r1, 8: r8} {
		if r < lo {
			t.Errorf("c=%d achieved %.2f× cap — below %.2f floor (too throttled)", c, r, lo)
		}
		if r > hi {
			t.Errorf("c=%d achieved %.2f× cap — above %.2f ceiling (limiter not aggregate; blew past with %d conns)",
				c, r, hi, c)
		}
	}
	// (b) The achieved rates for the two connection counts must be close — proving
	// the cap is GLOBAL, not split per connection. Two genuinely different rates
	// would mean the limiter is per-connection (8× faster at c=8) which is exactly
	// the bug §11.4 forbids. We bound their spread generously (±half) so jitter
	// can't flip a real-but-marginal aggregate cap into a failure.
	low, high := min(r1, r8), max(r1, r8)
	if high > 2*low {
		t.Errorf("aggregate not stable across -c: c=1 ratio %.2f vs c=8 ratio %.2f — global bucket not shared",
			r1, r8)
	}
}

// serveRangeServerRL is a plain range-supporting server (no artificial throttle)
// — kept self-contained so the ratelimit test doesn't entangle with the
// work-stealing server's slow shaping. The shared serveRangeServer in
// manager_test.go would serve just as well, but a private helper keeps the
// rate test's dependency surface explicit.
func serveRangeServerRL(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		start, end, ok := parseClientRangeS(rng, len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

var _ = fmt.Sprintf
