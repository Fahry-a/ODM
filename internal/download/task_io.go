package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"odm/internal/storage"
	"odm/internal/transport"
)

// worker pulls chunks from the given engine's queue and downloads them with
// retry on transient failures. Each chunk write uses storage.WriteAt so
// offset positioning is safe without locks. In the both profile each region
// has its own engine; single-profile tasks pass the one shared engine.
func (t *Task) worker(ctx context.Context, eng *Engine, wg *sync.WaitGroup, sink func(ProgressView)) {
	defer wg.Done()
	// Each worker represents one live connection. When it retires (queue empty,
	// chunk error, or ctx cancel), decrement the live-connection counter so the
	// UI's [xN] reflects the actual number of connections still downloading,
	// not the number originally launched. Guard against underflow. A worker
	// that retired via the drain check (retireIfAboveTarget) already
	// decremented the counter with its own CAS, so the defer skips it there.
	retired := false
	defer func() {
		if retired {
			return
		}
		for {
			old := t.conns.Load()
			if old <= 0 {
				return
			}
			if t.conns.CompareAndSwap(old, old-1) {
				return
			}
		}
	}()
	for {
		// Graceful drain: if we have more live workers than the target, this
		// worker retires before pulling the next chunk. The retirement is an
		// atomic CAS so exactly (live - target) workers retire no matter how
		// many check concurrently — without it, every worker that reads the
		// count while it is still above target would retire together, and the
		// queue could be left with no workers while chunks remain, letting
		// Start report a partial file as completed (and delete its control
		// file).
		if t.retireIfAboveTarget() {
			retired = true
			return
		}

		// Block while paused. Broadcast-close wake-up: Unpause releases every
		// worker blocked here, not just one (see Pause/Unpause).
		t.pauseGate(ctx)
		select {
		case <-ctx.Done():
			return
		default:
		}

		c, ok := eng.Next()
		if !ok {
			return // no more work
		}

		if err := t.downloadChunk(ctx, eng, c, sink); err != nil {
			if ctx.Err() != nil {
				t.errors.Add(1)
				// Persist completed chunks so partial progress survives
				// even if the process exits before Start's error path runs.
				t.persistControl()
				return
			}
			// A permanent failure (dead link: non-retryable 4xx) won't heal —
			// fail the task instead of requeueing.
			if isPermanent(err) {
				t.errors.Add(1)
				t.setState(StateError)
				t.logf("error", "chunk %d (off %d) failed permanently: %v", c.Index, c.Start, err)
				return
			}
			// Requeue the chunk so another worker gives it a fresh chance
			// instead of failing the whole task on one transient worker
			// failure. The chunk is dropped only after opts.Retry worker-level
			// passes (each pass running its own internal retry budget); beyond
			// that it's treated as permanently broken.
			if eng.Requeue(c, t.opts.Retry) {
				t.logf("warn", "chunk %d (off %d) failed, requeued for retry: %v", c.Index, c.Start, err)
				continue
			}
			t.errors.Add(1)
			t.setState(StateError)
			t.logf("error", "chunk %d (off %d) failed permanently: %v", c.Index, c.Start, err)
			return
		}
		eng.MarkDone(c)
		if sink != nil {
			sink(t.Snapshot())
		}
		// Periodic checkpoint: flush the control file on the count/time gate so
		// a crash/kill leaves a usable resume point (similar to aria2's periodic
		// .aria2 persist) without re-serialising the whole file every chunk.
		if t.checkpoint() {
			t.persistControl()
		}
	}
}

// retireIfAboveTarget attempts to retire this worker when the live connection
// count exceeds the target (AdjustConns reduction). The retirement is a CAS
// on t.conns: exactly the first (live - target) workers to call it win and
// exit, and every loser re-reads the (now-decremented) count and keeps
// working. This makes the drain exact regardless of how many workers check
// simultaneously. Returns true when the caller should exit (the counter was
// already decremented here).
// retireIfAboveTarget attempts to retire this worker when the live connection
// count exceeds the target (AdjustConns reduction). The retirement is a CAS
// on t.conns: exactly the first (live - target) workers to call it win and
// exit, and every loser re-reads the (now-decremented) count and keeps
// working. This makes the drain exact regardless of how many workers check
// simultaneously. Returns true when the caller should exit (the counter was
// already decremented here).
func (t *Task) retireIfAboveTarget() bool {
	for {
		live := t.conns.Load()
		if live <= t.connTarget.Load() {
			return false
		}
		if t.conns.CompareAndSwap(live, live-1) {
			return true
		}
	}
}

// isPermanent reports whether err is a retry-proof failure (a
// transport.StatusError flagged permanent anywhere in its Unwrap chain).
// isPermanent reports whether err is a retry-proof failure (a
// transport.StatusError flagged permanent anywhere in its Unwrap chain).
func isPermanent(err error) bool {
	var se transport.StatusError
	return errors.As(err, &se) && se.Permanent
}

// throttleOK restores the configured rate after a successful chunk — but only
// once throttleCooldown has passed since the latest 429. Without the cooldown,
// the first healthy worker (chunks complete every few hundred ms on an active
// download) would undo the halving while others are still being throttled.
// throttleOK restores the configured rate after a successful chunk — but only
// once throttleCooldown has passed since the latest 429. Without the cooldown,
// the first healthy worker (chunks complete every few hundred ms on an active
// download) would undo the halving while others are still being throttled.
func (t *Task) throttleOK() {
	last := t.lastThrottle.Load()
	if last == 0 || time.Since(time.Unix(0, last)) >= throttleCooldown {
		t.lim.ResetRate()
	}
}

// statusErr classifies an HTTP status from a ranged/plain GET: permanent for
// client errors except the two retryable ones (transport.IsPermanent),
// transient otherwise.
// statusErr classifies an HTTP status from a ranged/plain GET: permanent for
// client errors except the two retryable ones (transport.IsPermanent),
// transient otherwise.
func statusErr(msg string, status int) error {
	return transport.PermanentWrap(fmt.Errorf("%s: status %d", msg, status), status)
}

// downloadChunk fetches one chunk's byte-range (retrying up to opts.Retry times
// with exponential RetryWait backoff) and writes it to disk at the chunk's
// offset. `eng` supplies the region base (both profile) and transport client.
// downloadChunk fetches one chunk's byte-range (retrying up to opts.Retry times
// with exponential RetryWait backoff) and writes it to disk at the chunk's
// offset. `eng` supplies the region base (both profile) and transport client.
func (t *Task) downloadChunk(ctx context.Context, eng *Engine, c Chunk, sink func(ProgressView)) error {
	var lastErr error
	for attempt := range t.opts.Retry + 1 {
		if attempt > 0 {
			t.setState(StateRetrying)
			t.retries.Add(1)
			// Exponential backoff, capped so late attempts don't wait minutes:
			// RetryWait << attempt, max 30s. RetryWait 0 (tests, unset) keeps
			// zero wait — only a positive base is capped upward.
			wait := t.opts.RetryWait << min(attempt, 30)
			if t.opts.RetryWait > 0 && (wait > 30*time.Second || wait <= 0) {
				wait = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		t.setState(StateActive)
		// Per-attempt hasher: every byte written to disk is fed through it, but
		// the digest is only recorded on a fully-successful attempt — a hash is
		// never stored for a partially-written chunk.
		h := sha256.New()
		before := t.bytesDone.Load()
		err := t.fetchAndWrite(ctx, eng, c, sink, h)
		if err == nil {
			t.storeChunkHash(eng.AbsStart(c.Start), hex.EncodeToString(h.Sum(nil)))
			t.throttleOK()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Roll back this attempt's progress delta: fetchAndWrite already counted
		// the partial bytes via noteBytes, and the retry would count them AGAIN,
		// pushing BytesDone past TotalSize (honest-metrics fix; data integrity
		// is unaffected — the hasher is per-attempt too).
		if d := t.bytesDone.Load() - before; d > 0 {
			t.bytesDone.Add(-d)
			t.noteBytes(-d, sink)
		}
		// A 429 means the server is throttling THIS client's aggregate rate:
		// halve the shared limiter so every worker eases off, not just this
		// chunk's retries. The restore is cooldown-based (throttleOK), not
		// per-chunk-success — one healthy worker must not undo the halving
		// while others are still being 429'd.
		var se transport.StatusError
		if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
			if t.lim.BackOffSignal() {
				t.logf("warn", "server asked to slow down (429) — global rate halved")
			}
			t.lastThrottle.Store(time.Now().UnixNano())
		}
		// A permanent failure won't heal; skip the remaining attempts AND the
		// worker-level requeue passes (the worker checks isPermanent too).
		if isPermanent(err) {
			return err
		}
		lastErr = err
		t.logf("warn", "chunk %d attempt %d: %v", c.Index, attempt, err)
	}
	return lastErr
}

// chunkTimeoutCtx derives the per-chunk context: Timeout*10 (default 300s)
// so a stalled connection can't hang a worker forever.
// chunkTimeoutCtx derives the per-chunk context: Timeout*10 (default 300s)
// so a stalled connection can't hang a worker forever.
func (t *Task) chunkTimeoutCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	tmo := t.opts.Timeout * 10
	if tmo <= 0 {
		tmo = 300 * time.Second
	}
	return context.WithTimeout(ctx, tmo)
}

// contentRangeStartsWith reports whether a Content-Range header value
// ("bytes S-E/T") declares S == want. A 206 without a parseable start is
// treated as hostile — RFC 9110 requires the header on single-range 206s.
// contentRangeStartsWith reports whether a Content-Range header value
// ("bytes S-E/T") declares S == want. A 206 without a parseable start is
// treated as hostile — RFC 9110 requires the header on single-range 206s.
func contentRangeStartsWith(cr string, want int64) bool {
	rest, ok := strings.CutPrefix(cr, "bytes ")
	if !ok {
		return false
	}
	startStr, _, _ := strings.Cut(rest, "-")
	start, err := strconv.ParseInt(startStr, 10, 64)
	return err == nil && start == want
}

// fetchAndWrite does a single ranged GET and copies the body to disk, throttled
// by the global limiter and accounting bytes into progress. h receives every
// byte written to disk so the caller can record the chunk's SHA-256 on success.
// eng supplies the region base (both profile) and transport client.
// fetchAndWrite does a single ranged GET and copies the body to disk, throttled
// by the global limiter and accounting bytes into progress. h receives every
// byte written to disk so the caller can record the chunk's SHA-256 on success.
// eng supplies the region base (both profile) and transport client.
func (t *Task) fetchAndWrite(ctx context.Context, eng *Engine, c Chunk, sink func(ProgressView), h hash.Hash) error {
	// Per-chunk timeout prevents a stalled connection from hanging a worker
	// forever (see chunkTimeoutCtx). If the timeout fires, the chunk is
	// retried by the caller (downloadChunk).
	chunkCtx, chunkCancel := t.chunkTimeoutCtx(ctx)
	defer chunkCancel()

	pr := t.probe.Load()
	// Sizeless single-stream chunk: plain GET, no Range.
	if pr.TotalSize < 0 || (pr.SingleStream && c.Start == 0 && c.Index == 0) {
		return t.fetchWhole(chunkCtx, c, sink, h)
	}

	// Absolute offsets: region2 (both) starts at base, so Range and disk
	// write use base+rel.
	absStart := eng.AbsStart(c.Start)
	absEnd := c.End
	if absEnd >= 0 {
		absEnd = eng.AbsStart(c.End)
	}
	if absEnd < 0 {
		absEnd = -1
	}
	// Mirror rotation: each chunk request takes the next URL in the mirror
	// list (round-robin across all workers), so a batch of chunks spreads over
	// every source. The primary URL stays in the rotation too — it's slot 0.
	url := pr.FinalURL
	ifRange := t.resumeETag
	if n := len(t.opts.Mirrors); n > 0 {
		i := t.mirrorIdx.Add(1) % uint64(n+1)
		if i > 0 {
			url = t.opts.Mirrors[i-1]
			// If-Range carries the PRIMARY's ETag — a mirror with a different
			// ETag scheme would answer 200 and be misread as resource drift.
			// Mirrors get no If-Range; their Content-Range validation still
			// guards every response.
			ifRange = ""
			t.logf("info", "chunk %d from mirror %s", c.Index, url)
		}
	}
	resp, err := eng.Client().GetRange(chunkCtx, url, absStart, absEnd, ifRange)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A 200 here means the server answered outside this run's byte layout:
	// either it ignored Range entirely (writing the full body at the chunk's
	// offset would corrupt the file — chunk 3's slot would hold bytes from
	// position 0), or an If-Range resume detected that the resource changed
	// since the interrupted run. Both are drift: retrying won't fix it, so
	// this is classified PERMANENT explicitly (a plain statusErr would leave
	// 200 retryable — the old behaviour caused a retry-storm before failing).
	if resp.StatusCode != http.StatusPartialContent {
		transport.SkipBody(resp.Body)
		op := "server ignored range request"
		if t.resumeETag != "" {
			op = "resume target changed since last run (If-Range mismatch)"
		}
		return transport.StatusError{
			Err:       fmt.Errorf("%s at offset %d (status %d)", op, absStart, resp.StatusCode),
			Status:    resp.StatusCode,
			Permanent: true,
		}
	}
	// A 206 whose Content-Range doesn't start at the requested offset is the
	// same corruption vector as a 200: the bytes belong somewhere else on
	// disk. Verify before a single byte is written; like the 200 case, no
	// amount of retrying fixes a server that lies about its own layout.
	if cr := resp.Header.Get("Content-Range"); !contentRangeStartsWith(cr, absStart) {
		transport.SkipBody(resp.Body)
		return transport.StatusError{
			Err:       fmt.Errorf("server sent wrong range for offset %d (Content-Range %q)", absStart, cr),
			Status:    http.StatusExpectationFailed,
			Permanent: true,
		}
	}

	body := resp.Body
	if !t.lim.Unlimited() {
		body = io.NopCloser(t.lim.Reader(chunkCtx, body))
	}
	if tl := t.taskLim.Load(); tl != nil && !tl.Unlimited() {
		body = io.NopCloser(tl.Reader(chunkCtx, body))
	}

	// Copy chunk to disk at the absolute offset (base+Start for region2) using
	// a small buffer; the WriteAt positions the write regardless of file pointer.
	buf := make([]byte, 64*1024)
	var off int64
	n, err := copyChunkFrom(body, t.disk, absStart, buf, &off, h, func(delta int64) {
		t.noteBytes(delta, sink)
	})
	if err != nil {
		return err
	}
	// Validate we got the expected chunk size when it's bounded. A mismatch
	// after a 206-with-verified-Content-Range means the server is lying about
	// its own layout (the lying-206 case) — no amount of retrying fixes that,
	// so classify it permanent like the other drift signals.
	if c.End >= 0 && pr.TotalSize > 0 && off != (c.End-c.Start+1) {
		err := fmt.Errorf("chunk %d short read: got %d want %d", c.Index, off, c.End-c.Start+1)
		return transport.PermanentWrap(err, http.StatusExpectationFailed)
	}
	_ = n
	return nil
}

// fetchWhole handles the sizeless or range-less single-stream case:
// plain GET of the whole resource, sequential write at offset 0; bytes are
// counted into progress but the total stays -1 so the UI shows "sizeless".
// h receives every byte written to disk (the whole-file chunk's SHA-256).
// fetchWhole handles the sizeless or range-less single-stream case:
// plain GET of the whole resource, sequential write at offset 0; bytes are
// counted into progress but the total stays -1 so the UI shows "sizeless".
// h receives every byte written to disk (the whole-file chunk's SHA-256).
func (t *Task) fetchWhole(ctx context.Context, _ Chunk, sink func(ProgressView), h hash.Hash) error {
	// Per-chunk timeout prevents a stalled connection from hanging forever.
	chunkCtx, chunkCancel := t.chunkTimeoutCtx(ctx)
	defer chunkCancel()

	pr := t.probe.Load()
	req, err := t.client.NewGetRequest(chunkCtx, pr.FinalURL)
	if err != nil {
		return err
	}
	resp, err := t.client.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Non-2xx here is an error, not an empty file: writing a 403/404/500 body
	// to disk would silently "complete" the task with garbage data. The ranged
	// path validates status inside GetRange; the whole-file fallback (every
	// non-range-capable and sizeless URL) must do the same.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		transport.SkipBody(resp.Body)
		return statusErr(fmt.Sprintf("GET %s", pr.FinalURL), resp.StatusCode)
	}
	body := resp.Body
	if !t.lim.Unlimited() {
		body = io.NopCloser(t.lim.Reader(chunkCtx, body))
	}
	if tl := t.taskLim.Load(); tl != nil && !tl.Unlimited() {
		body = io.NopCloser(tl.Reader(chunkCtx, body))
	}
	buf := make([]byte, 64*1024)
	_, err = copyChunkFrom(body, t.disk, 0, buf, new(int64), h, func(delta int64) {
		t.noteBytes(delta, sink)
	})
	return err
}

// copyChunkFrom copies r into w.WriteAt at base offset, advancing a local
// offset counter, feeding every written byte through h (may be nil), and
// calling onProgress for each read's delta. Returns total n.
// copyChunkFrom copies r into w.WriteAt at base offset, advancing a local
// offset counter, feeding every written byte through h (may be nil), and
// calling onProgress for each read's delta. Returns total n.
func copyChunkFrom(r io.Reader, w *storage.File, base int64, buf []byte, off *int64, h hash.Hash, onProgress func(int64)) (int64, error) {
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.WriteAt(buf[:n], base+*off); werr != nil {
				return total, werr
			}
			if h != nil {
				// sha256.Write never fails; ignore the count/error.
				_, _ = h.Write(buf[:n])
			}
			*off += int64(n)
			total += int64(n)
			if onProgress != nil {
				onProgress(int64(n))
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// noteBytes updates the rolling speed measure and atomics and, when the ~100ms
// progress gate elapses, pushes a fresh snapshot through sink so the UI/RPC
// feeder sees live bytes-done *during* a long stream rather than only at chunk
// boundaries. The gate is shared under rmMu across this task's workers, so the
// sink fires ~10×/s per task regardless of how many connections are active
// . sink may be nil (the Manager.Run test path
// and RPC single-task probes pass nil); calling is skipped in that case.
// noteBytes updates the rolling speed measure and atomics and, when the ~100ms
// progress gate elapses, pushes a fresh snapshot through sink so the UI/RPC
// feeder sees live bytes-done *during* a long stream rather than only at chunk
// boundaries. The gate is shared under rmMu across this task's workers, so the
// sink fires ~10×/s per task regardless of how many connections are active
// . sink may be nil (the Manager.Run test path
// and RPC single-task probes pass nil); calling is skipped in that case.
func (t *Task) noteBytes(delta int64, sink func(ProgressView)) {
	if delta < 0 {
		return
	}
	t.bytesDone.Add(delta)
	t.rmMu.Lock()
	flushed := t.rm.tick(delta)
	s := t.rm.bps
	t.rmMu.Unlock()
	t.speed.Store(s)
	if flushed && sink != nil {
		sink(t.Snapshot())
	}
}

// emitFinal pushes one last snapshot carrying the terminal state (Completed
// or Error) through the progressSink before Start returns. This is the root
// cause fix for the "Total: 0/0 / completed line vanishes" bug: the
// scheduler's handleComplete deletes a finished task from its live map the
// instant Start returns and never forwards the terminal snapshot on its own,
// so without this final emit the last snapshot the UI cached is still Active
// (or the task simply disappears). Firing here — while the task is still in
// the scheduler's live map and before compl is signalled — guarantees a
// frame with the real final state reaches the UI. The UI's Renderer also
// retains the cached terminal state defensively (see internal/ui Frame's
// vanish handling), so the two fixes are belt-and-suspenders. sink is nil on
// the Manager.Run test path and the RPC single-task probe path; in that case
// this call is a no-op.
