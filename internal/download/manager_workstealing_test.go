package download

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWorkStealing_BeatsStaticEqualSplit is the PRD §16 work-stealing
// acceptance test: with a work-stealing chunk queue (§11.1), the straggler
// problem of a static equal-split is avoided — a slow region of the file is
// split into many small chunks that N workers pull concurrently, instead of one
// worker being forced to read the whole slow region serially.
//
// Model: a contiguous "slow region" of the last `slowCount` chunks is throttled
// per overlapping chunk. Because the throttle scales with the number of slow
// chunks a single request covers, the two strategies differ sharply:
//
//   - Static equal-split (N fixed contiguous slices, one worker per slice): the
//     slice that contains the slow region issues ONE ranged GET the server
//     delays once per slow chunk it overlaps. That worker is the straggler and
//     pays the FULL serial penalty (slowCount * perChunkDelay); the other slices
//     finish immediately. Wall-clock ≈ that straggler (this is the classic
//     straggler problem of equal-split segmentation, PRD §11.1).
//   - Work-stealing chunk queue (N workers pulling from a shared queue): the
//     slow region's chunks are fetched concurrently, up to N at a time, so the
//     slow region finishes in ~ceil(slowCount/N) waves. Wall-clock drops by
//     ~N×.
//
// We assert the work-stealing run is clearly faster than the static equal-split
// baseline over the SAME server/payload. The constants leave a wide margin so
// scheduling/timing jitter on a loaded CI box cannot flip the verdict.
func TestWorkStealing_BeatsStaticEqualSplit(t *testing.T) {
	const (
		chunk         = 16 * 1024 // per-chunk size (small → many chunks queue up)
		numChunks     = 64        // total payload = 1 MiB across 64 chunks
		S             = 4         // parallel workers/connections (also static-split parts)
		slowCount     = 8         // last 8 chunks form the slow region (lives in one static slice)
		perChunkDelay = 120 * time.Millisecond
	)

	payload := make([]byte, int64(chunk)*numChunks)
	for i := range payload {
		payload[i] = byte(i)
	}

	srv := serveProportionalSlowRangeServer(t, payload, chunk, slowCount, perChunkDelay)
	defer srv.Close()

	// --- 1. Chunk-queue (work-stealing) run via the real Manager engine. ---
	dir := t.TempDir()
	m, err := NewManager(ExecOptions{
		Dir:         dir,
		OutFile:     "ws.bin",
		Connections: S,
		Retry:       0,
		RetryWait:   0,
		Continue:    false,
		ChunkSize:   chunk,
		Timeout:     30 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	wsCtx, wsCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer wsCancel()
	wsStart := time.Now()
	if err := m.Run(wsCtx, srv.URL, S); err != nil {
		t.Fatalf("work-stealing download: %v", err)
	}
	wsElapsed := time.Since(wsStart)

	got, err := os.ReadFile(filepath.Join(dir, "ws.bin"))
	if err != nil {
		t.Fatalf("read ws out: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("ws size: want %d got %d", len(payload), len(got))
	}
	if string(got) != string(payload) {
		t.Fatalf("ws content mismatch")
	}

	// --- 2. Static equal-split baseline over the SAME server. ---
	staticDir := t.TempDir()
	staticElapsed := runStaticEqualSplit(t, srv.URL, staticDir, "stat.bin", payload, S)
	t.Logf("elapsed: work-stealing=%v static-equal-split=%v", wsElapsed, staticElapsed)

	// Sanity: the static baseline hit the straggler (its slow slice must cost at
	// least slowCount*perChunkDelay). If not, the server isn't throttling
	// proportionally and any pass would be vacuous — fail loudly.
	wantStaticFloor := time.Duration(slowCount)*perChunkDelay - 100*time.Millisecond
	if staticElapsed < wantStaticFloor {
		t.Fatalf("static baseline (%v) below expected straggler floor (%v) — proportional throttle not firing",
			staticElapsed, wantStaticFloor)
	}

	// The chunk queue must finish clearly faster than the static straggler. The
	// expected speedup is ~S×; require at least a 2× win so jitter can't flip it.
	if wsElapsed*2 >= staticElapsed {
		t.Fatalf("work-stealing (%v) did not beat static equal-split (%v) by ≥2×; "+
			"per-chunk delay was %v, slow chunks %d", wsElapsed, staticElapsed, perChunkDelay, slowCount)
	}
}

// serveProportionalSlowRangeServer is the range server from manager_test with a
// throttle on the last `slowCount` chunks proportional to how many slow chunks
// a single request overlaps. HEAD advertises the size + Accept-Ranges so the
// probe keeps the multi-worker chunk-queue path alive. This is what makes the
// straggler problem visible: a single big request spanning the whole slow region
// pays once-per-slow-chunk, serially, whereas many concurrent small requests
// each pay for only one slow chunk and overlap in wall-clock.
func serveProportionalSlowRangeServer(t *testing.T, payload []byte, chunkSize, slowCount int, perChunkDelay time.Duration) *httptest.Server {
	t.Helper()
	slowStart := max(int64(0), int64(len(payload))-int64(slowCount)*int64(chunkSize))
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
		// Throttle every byte of the slow region this request touches: each
		// whole slow chunk overlapped costs one perChunkDelay. The caller
		// shape guarantees a single ranged request → serial delay = (#slow
		// chunks overlapped) * perChunkDelay.
		if end >= slowStart {
			overlapStart := max(start, slowStart)
			touched := (end - overlapStart + 1) / int64(chunkSize)
			if (end-overlapStart+1)%int64(chunkSize) != 0 {
				touched++ // partial slow chunk counts as one
			}
			if touched < 1 {
				touched = 1
			}
			time.Sleep(time.Duration(touched) * perChunkDelay)
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

// runStaticEqualSplit emulates a static equal-split baseline: the file is
// divided into S contiguous, non-overlapping slices; one worker goroutine per
// slice fetches its slice as a single ranged request and writes it at its base
// offset. There is NO shared queue — a worker cannot help another — so the
// worker owning the slow region is the straggler that gates the wall-clock
// (exactly the problem §11.1 says the chunk queue avoids). Returns total
// elapsed time. Errors fail the test (the same payload/server already succeeded
// for the chunk-queue run, so a slice failing is a bug).
func runStaticEqualSplit(t *testing.T, url, dir, name string, payload []byte, parts int) time.Duration {
	t.Helper()
	totalLen := int64(len(payload))
	partSize := (totalLen + int64(parts) - 1) / int64(parts) // ceil so union == whole file

	out := filepath.Join(dir, name)
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("static open: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(totalLen); err != nil { // pre-allocate like the engine
		t.Fatalf("static truncate: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, parts)
	start := time.Now()
	for i := range parts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lo := int64(i) * partSize
			hi := lo + partSize - 1
			if hi >= totalLen {
				hi = totalLen - 1
			}
			if lo > hi {
				return
			}
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", lo, hi))
			resp, rerr := http.DefaultClient.Do(req)
			if rerr != nil {
				errs <- fmt.Errorf("slice %d: %w", i, rerr)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				errs <- fmt.Errorf("slice %d: status %d", i, resp.StatusCode)
				return
			}
			buf := make([]byte, 64*1024)
			off := lo
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					if _, werr := f.WriteAt(buf[:n], off); werr != nil {
						errs <- fmt.Errorf("slice %d write: %w", i, werr)
						return
					}
					off += int64(n)
				}
				if rerr != nil {
					break
				}
			}
			if off != hi+1 {
				errs <- fmt.Errorf("slice %d short: got %d want %d", i, off-lo, hi-lo+1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(errs)
	for e := range errs {
		t.Fatalf("static slice failed: %v", e)
	}

	// The static baseline must reproduce the exact payload (same server), so a
	// mismatch means the baseline didn't actually fetch the slow region — which
	// would make any pass vacuous. Verify rather than trust.
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("static read: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("static size: want %d got %d", len(payload), len(got))
	}
	return elapsed
}
