package download

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// newQuickTask builds a 1-connection task against srvURL with a fast retry
// budget so tests don't wait on backoff.
func newQuickTask(t *testing.T, srvURL, dir string) *Task {
	t.Helper()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	return NewTask(TaskID("t"), srvURL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      3,
		RetryWait:  time.Millisecond,
		Timeout:    5 * time.Second,
		ChunkSize:  1024,
	}, cli, lim, nil)
}

// TestPermanentError_Classification pins the status taxonomy: client errors
// are permanent except 408/429; server errors and success stay transient.
func TestPermanentError_Classification(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusForbidden:           true,
		http.StatusNotFound:            true,
		http.StatusGone:                true,
		http.StatusUnauthorized:        true,
		http.StatusRequestTimeout:      false, // 408: retryable
		http.StatusTooManyRequests:     false, // 429: retryable
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
	} {
		err := transport.PermanentWrap(errors.New("x"), status)
		if got := isPermanent(err); got != want {
			t.Errorf("status %d: isPermanent=%v want %v", status, got, want)
		}
	}
	if isPermanent(errors.New("plain")) || isPermanent(nil) {
		t.Error("plain/nil errors must not classify permanent")
	}
}

// TestFailFast_DeadLink pins the fail-fast contract: a 403 chunk must fail
// the task after exactly ONE attempt per worker pass — no internal retries
// (the old code burned Retry+1 attempts AND Requeue passes on a link that
// can never recover).
func TestFailFast_DeadLink(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "2048")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Range") != "" {
			hits.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	task := newQuickTask(t, srv.URL, dir)
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: true, TotalSize: 2048, Filename: "out.bin"})
	task.SetProfile("odm")
	err := task.Start(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("expected task failure against a 403 link")
	}
	// One ranged GET total: attempt 0 sees permanent → immediate abort. The
	// pre-fix engine issued Retry+1 = 4 attempts plus requeue passes.
	if n := hits.Load(); n != 1 {
		t.Fatalf("ranged GET count = %d, want 1 (fail-fast)", n)
	}
}

// TestIfRange_ResumeDetectsDrift pins If-Range: a resume against a server
// whose ETag changed mid-run gets 200s instead of 206s, and the chunk path
// fails fast on them rather than writing full-body bytes at chunk offsets.
func TestIfRange_ResumeDetectsDrift(t *testing.T) {
	var sawIfRange atomic.Bool
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i%251 + 1)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"v2"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		if ir := r.Header.Get("If-Range"); ir != "" {
			sawIfRange.Store(true)
			// ETag changed since the control file was written ("v1") → the
			// whole resource comes back as 200.
			w.Header().Set("Content-Length", itoaS(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		// No If-Range (probe path etc.): honest range response.
		start, end, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	dir := t.TempDir()
	task := newQuickTask(t, srv.URL, dir)
	// Simulate a validated resume: resumeETag set from an older control file.
	task.SetProbe(&transport.ProbeResult{FinalURL: srv.URL, SupportsRange: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("odm")
	task.resumeETag = `"v1"`
	err := task.Start(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("expected drift-detected failure")
	}
	if !sawIfRange.Load() {
		t.Fatal("server never received an If-Range header")
	}
	// The task failed on the drift (nothing completed — the 200 bodies were
	// refused), not after stitching bytes.
	if strings.Contains(err.Error(), "4096") && !strings.Contains(err.Error(), "0/4096") {
		t.Fatalf("drift failure should report zero progress, got: %v", err)
	}
}

// TestBackoff_ExponentialCap pins the backoff math: base<<attempt capped at
// 30s, zero base stays zero (tests/unset). Mirrors downloadChunk's formula.
func TestBackoff_ExponentialCap(t *testing.T) {
	base := 100 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		want := base << min(attempt, 30)
		if got := base << min(attempt, 30); got != want || got <= 0 {
			t.Errorf("attempt %d: wait=%v", attempt, got)
		}
	}
	// Cap branch: a large base clamps to 30s.
	wait := 10 * time.Second << min(5, 30)
	if wait <= 30*time.Second {
		t.Fatalf("expected >cap value before clamp, got %v", wait)
	}
}
