package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
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
		// Release the in-flight record on every exit from this iteration; the
		// paths that finish/mark the piece delete the record themselves, so
		// this only covers the early returns (error / ctx cancel).
		deferRelease := func() { eng.Release(c) }

		if err := t.downloadChunk(ctx, eng, c, sink); err != nil {
			var sp *splitPointError
			if errors.As(err, &sp) {
				rs, re, rootDone := eng.SplitDone(c, sp.written)
				deferRelease()
				if rootDone {
					t.hashCompleteRoot(eng, rs, re)
				}
				continue
			}
			deferRelease()
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
		deferRelease()
		rs, re, rootDone := eng.MarkDone(c)
		if rootDone && !eng.WholeChunk(c) {
			// A root chunk completed via sub-ranges (tail-split): hash the full
			// span from disk so a later --continue verifies it instead of
			// silently downgrading to server-side compare.
			t.hashCompleteRoot(eng, rs, re)
		}
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
func isPermanent(err error) bool {
	var se transport.StatusError
	return errors.As(err, &se) && se.Permanent
}

// throttleOK restores the configured rate only after the shared limiter's
// global adaptive cooldown has expired.
func (t *Task) throttleOK() {
	t.lim.ThrottleOK()
}

// chunkResult reports one attempt's progress: written bytes are already on disk
// and hashed. resumable says a retry may continue at start+written instead of
// re-downloading the chunk from its beginning (false for single-stream bodies,
// which have no byte-range to ask for).
type chunkResult struct {
	written   int64
	resumable bool
}

// Retry timing: the first retry waits at most RetryWait (not 2× it), jitter
// keeps workers from retrying in lockstep, backoff is capped at 10s, and a
// server-provided Retry-After wins over the local backoff.
const (
	maxRetryBackoff = 10 * time.Second
	maxRetryAfter   = 60 * time.Second
)

// retryDelay returns the wait before retry `attempt` (1-based): exponential
// from base with equal jitter (half fixed, half random), capped at 10s. A base
// of 0 means no wait (tests, --retry-wait 0).
func retryDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < attempt && d < maxRetryBackoff; i++ {
		d *= 2
	}
	if d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleepCtx waits d or until ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// defaultIdleTimeout caps how long a stalled body read may stay silent. A total
// per-chunk deadline is wrong on slow links (a 4 MiB chunk at 0.7 MB/s already
// takes ~6s and a bigger chunk far longer), so only a silent socket counts as a
// failure. Overridable in tests through TaskOptions.Timeout (min(timeout, 20s)).
const defaultIdleTimeout = 20 * time.Second

// copyBufSize is the per-chunk copy buffer for the hot read/write loop.
// 256 KiB keeps Read/Write syscall and limiter-token overhead off the
// critical path on fast links (4× fewer round trips than 64 KiB) while
// remaining negligible against the connection budget (32 conns × 256 KiB).
const copyBufSize = 256 << 10

// idleReadTimeout derives the stall detector from the configured network
// timeout: min(--timeout, 20s), with 20s as the fallback.
func idleReadTimeout(tmo time.Duration) time.Duration {
	if tmo <= 0 || tmo > defaultIdleTimeout {
		return defaultIdleTimeout
	}
	return tmo
}

// idleReader cancels the attempt's context when a pending Read makes no
// progress for d. The timer is armed on read entry and disarmed on return, so
// it never fires while no read is pending (e.g. the rate limiter is waiting for
// tokens and there is no socket at all).
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (ir *idleReader) Read(p []byte) (int, error) {
	ir.timer.Reset(ir.d)
	n, err := ir.r.Read(p)
	ir.timer.Stop()
	return n, err
}

// guardIdle disarms an inactivity timer over r for the course of the attempt:
// the timer is armed only while a socket read is in flight; if a pending read
// goes silent for the idle timeout, cancel fires (aborting the HTTP body read)
// so the retry loop can resume the chunk. Between reads (e.g. while the rate
// limiter waits for tokens) no timer is armed — a slow-but-healthy transfer is
// never aborted. The returned stop func must run when the attempt ends.
func (t *Task) guardIdle(r io.Reader, cancel context.CancelFunc) (io.Reader, func()) {
	d := idleReadTimeout(t.opts.Timeout)
	timer := time.AfterFunc(d, cancel)
	timer.Stop() // disarmed until the next read
	return &idleReader{r: r, timer: timer, d: d}, func() { timer.Stop() }
}

// downloadChunk fetches one chunk's byte-range (retrying up to opts.Retry times
// with jittered backoff) and writes it to disk at the chunk's offset. A failed
// attempt keeps the bytes it already wrote: the retry asks for the remaining
// range instead of throwing away good progress. `eng` supplies the region base
// (both profile) and transport client.
func (t *Task) downloadChunk(ctx context.Context, eng *Engine, c Chunk, sink func(ProgressView)) error {
	var lastErr error
	h := sha256.New()
	var done int64 // bytes of this chunk already written and hashed
	// rollback drops this chunk's partial progress from the byte counter. An
	// abandoned chunk is never marked done (a requeue re-downloads it from 0,
	// and the control file only lists completed chunks), so those bytes would
	// otherwise be counted a second time by the next worker.
	rollback := func() {
		if done <= 0 {
			return
		}
		t.bytesDone.Add(-done)
		done = 0
		if sink != nil {
			sink(t.Snapshot())
		}
	}
	for attempt := range t.opts.Retry + 1 {
		if attempt > 0 {
			t.setState(StateRetrying)
			t.retries.Add(1)
			wait := retryDelay(t.opts.RetryWait, attempt)
			var se transport.StatusError
			if errors.As(lastErr, &se) && se.RetryAfter > 0 {
				wait = min(se.RetryAfter, maxRetryAfter)
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
		}
		t.setState(StateActive)
		res, err := t.fetchAndWrite(ctx, eng, c, sink, h, done)
		if err == nil {
			// Only a piece that covers the layout chunk in full carries the
			// whole root's hash; split pieces contribute to a root that is
			// hashed from disk once it is complete (nothing was discarded —
			// see worker).
			if eng.WholeChunk(c) {
				t.storeChunkHash(eng.AbsStart(c.Start), hex.EncodeToString(h.Sum(nil)))
			}
			t.throttleOK()
			return nil
		}
		if errors.Is(err, errChunkSplit) {
			// The idle-tail split boundary was reached on purpose. Return a
			// dedicated signal (not a failure) so the worker stops this piece
			// and requeues the unwritten remainder.
			return &splitPointError{written: done + res.written}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if res.resumable {
			done += res.written
		} else if res.written > 0 {
			// A single-stream body has no range to resume from: the bytes must
			// be read again, so roll the progress back and restart the hash.
			t.bytesDone.Add(-res.written)
			if sink != nil {
				sink(t.Snapshot())
			}
			h = sha256.New()
			done = 0
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
		}
		// A permanent failure won't heal; skip the remaining attempts AND the
		// worker-level requeue passes (the worker checks isPermanent too).
		if isPermanent(err) {
			rollback()
			return err
		}
		lastErr = err
		t.logf("warn", "chunk %d attempt %d: %v", c.Index, attempt, err)
	}
	rollback()
	return lastErr
}

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

// fetchAndWrite does one ranged GET and copies the body to disk, throttled by
// the global limiter and accounting bytes into progress. resumeFrom is the
// number of bytes of this chunk already on disk, so a retry asks only for the
// remaining range; h must already contain those bytes' hash. eng supplies the
// region base (both profile) and transport client.
func (t *Task) fetchAndWrite(ctx context.Context, eng *Engine, c Chunk, sink func(ProgressView), h hash.Hash, resumeFrom int64) (chunkResult, error) {
	// The idle guard cancels this context when the socket goes silent, which
	// aborts the body read and lets downloadChunk resume the chunk.
	chunkCtx, chunkCancel := context.WithCancel(ctx)
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
	} else {
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
	from := absStart + resumeFrom
	if resumeFrom > 0 {
		end := "EOF"
		if absEnd >= 0 {
			end = fmt.Sprintf("%d", absEnd+1)
		}
		t.logf("info", "chunk %d resuming at byte %d/%s", c.Index, from, end)
	}
	resp, err := eng.Client().GetRange(chunkCtx, url, from, absEnd, ifRange)
	if err != nil {
		return chunkResult{resumable: true}, err
	}
	defer resp.Body.Close()
	// A 200 here means the server answered outside this run's byte layout:
	// either it ignored Range entirely (writing the full body at the chunk's
	// offset would corrupt the file — chunk 3's slot would hold bytes from
	// position 0), or an If-Range resume detected that the resource changed
	// since the interrupted run. Both are drift: retrying won't fix it, so
	// this is classified PERMANENT explicitly (a generic status wrap would
	// leave 200 retryable — the old behaviour caused a retry-storm before
	// failing).
	if resp.StatusCode != http.StatusPartialContent {
		transport.SkipBody(resp.Body)
		op := "server ignored range request"
		if t.resumeETag != "" {
			op = "resume target changed since last run (If-Range mismatch)"
		}
		return chunkResult{resumable: true}, transport.StatusError{
			Err:       fmt.Errorf("%s at offset %d (status %d)", op, from, resp.StatusCode),
			Status:    resp.StatusCode,
			Permanent: true,
		}
	}
	// A 206 whose Content-Range doesn't start at the requested offset is the
	// same corruption vector as a 200: the bytes belong somewhere else on
	// disk. Verify before a single byte is written; like the 200 case, no
	// amount of retrying fixes a server that lies about its own layout.
	if cr := resp.Header.Get("Content-Range"); !contentRangeStartsWith(cr, from) {
		transport.SkipBody(resp.Body)
		return chunkResult{resumable: true}, transport.StatusError{
			Err:       fmt.Errorf("server sent wrong range for offset %d (Content-Range %q)", from, cr),
			Status:    http.StatusExpectationFailed,
			Permanent: true,
		}
	}

	var body io.Reader = resp.Body
	body, stopIdle := t.guardIdle(body, chunkCancel)
	defer stopIdle()
	if !t.lim.Unlimited() {
		body = t.lim.Reader(chunkCtx, body)
	}
	if tl := t.taskLim.Load(); tl != nil && !tl.Unlimited() {
		body = tl.Reader(chunkCtx, body)
	}

	trk := eng.Track(c)
	buf := make([]byte, copyBufSize)
	off := resumeFrom
	_, err = copyChunkFrom(body, t.disk, absStart, buf, &off, h, func(delta int64) {
		t.noteBytes(delta, sink)
	}, trk)
	if err != nil {
		if errors.Is(err, errChunkSplit) {
			return chunkResult{written: off - resumeFrom, resumable: true}, err
		}
		return chunkResult{written: off - resumeFrom, resumable: true}, err
	}
	if c.End >= 0 && pr.TotalSize > 0 && off != (c.End-c.Start+1) {
		err := fmt.Errorf("chunk %d short read: got %d want %d", c.Index, off, c.End-c.Start+1)
		return chunkResult{written: off - resumeFrom, resumable: true}, transport.PermanentWrap(err, http.StatusExpectationFailed)
	}
	return chunkResult{written: off - resumeFrom, resumable: true}, nil
}

func (t *Task) fetchWhole(ctx context.Context, _ Chunk, sink func(ProgressView), h hash.Hash) (chunkResult, error) {
	chunkCtx, chunkCancel := context.WithCancel(ctx)
	defer chunkCancel()

	pr := t.probe.Load()
	req, err := t.client.NewGetRequest(chunkCtx, pr.FinalURL)
	if err != nil {
		return chunkResult{}, err
	}
	resp, err := t.client.HTTP.Do(req)
	if err != nil {
		return chunkResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryAfter := transport.ParseRetryAfter(resp.Header.Get("Retry-After"))
		transport.SkipBody(resp.Body)
		return chunkResult{}, transport.StatusError{
			Err:        fmt.Errorf("GET %s: status %d", pr.FinalURL, resp.StatusCode),
			Status:     resp.StatusCode,
			Permanent:  transport.IsPermanent(resp.StatusCode),
			RetryAfter: retryAfter,
		}
	}
	var body io.Reader = resp.Body
	body, stopIdle := t.guardIdle(body, chunkCancel)
	defer stopIdle()
	if !t.lim.Unlimited() {
		body = t.lim.Reader(chunkCtx, body)
	}
	if tl := t.taskLim.Load(); tl != nil && !tl.Unlimited() {
		body = tl.Reader(chunkCtx, body)
	}
	buf := make([]byte, copyBufSize)
	var off int64
	_, err = copyChunkFrom(body, t.disk, 0, buf, &off, h, func(delta int64) {
		t.noteBytes(delta, sink)
	}, nil)
	if err != nil {
		return chunkResult{written: off}, err
	}
	// A known-size single-stream resource must end exactly at its probed size.
	// EOF before that point is a failed download, not a successful partial file.
	if pr.TotalSize > 0 && off != pr.TotalSize {
		return chunkResult{written: off}, transport.PermanentWrap(
			fmt.Errorf("short read: got %d bytes, want %d", off, pr.TotalSize),
			http.StatusExpectationFailed,
		)
	}
	return chunkResult{written: off}, nil
}

func copyChunkFrom(r io.Reader, w *storage.File, base int64, buf []byte, off *int64, h hash.Hash, onProgress func(int64), trk *activePiece) (int64, error) {
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if trk != nil {
				// A requested tail-split boundary stops this piece: write at
				// most up to the boundary, then re-enter the queue.
				if stop := trk.stopAfter.Load(); stop > 0 {
					remaining := stop - trk.written.Load()
					if remaining <= 0 {
						return total, errChunkSplit
					}
					if int64(n) > remaining {
						n = int(remaining)
					}
				}
			}
			// Account for the bytes BEFORE they hit the disk: a concurrent
			// tail-split decision reads trk.written right after its CAS and
			// relies on it seeing in-flight bytes, not just landed ones.
			if trk != nil {
				trk.written.Add(int64(n))
			}
			if _, werr := w.WriteAt(buf[:n], base+*off); werr != nil {
				return total, werr
			}
			if h != nil {
				_, _ = h.Write(buf[:n])
			}
			*off += int64(n)
			total += int64(n)
			if onProgress != nil {
				onProgress(int64(n))
			}
			if trk != nil {
				wbytes := trk.written.Load()
				// A requested tail-split boundary stops this piece; the
				// remainder re-enters the queue and the bytes already
				// counted here are final progress, not to be rolled back.
				if stop := trk.stopAfter.Load(); stop > 0 && wbytes >= stop {
					return total, errChunkSplit
				}
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

// hashCompleteRoot reads the full span [absStart, absEnd] (inclusive) from
// disk and stores its SHA-256 as the root chunk's resume hash. Called when a
// layout chunk completed via tail-split sub-ranges — the incremental hash the
// copy loop keeps during the piece only covers a strict sub-range, so the
// hash has to come from disk (this runs once per completed root, not on the
// copy hot loop).
func (t *Task) hashCompleteRoot(eng *Engine, rs, re int64) {
	absStart, absEnd := eng.AbsStart(rs), eng.AbsStart(re)
	if absEnd < absStart || t.disk == nil {
		return
	}
	h := sha256.New()
	buf := make([]byte, copyBufSize)
	for off := absStart; off <= absEnd; {
		n := int64(len(buf))
		if off+n-1 > absEnd {
			n = absEnd - off + 1
		}
		if _, err := t.disk.ReadAt(buf[:n], off); err != nil {
			t.logf("warn", "could not hash resume span [%d,%d]: %v", rs, re, err)
			return
		}
		_, _ = h.Write(buf[:n])
		off += n
	}
	t.storeChunkHash(absStart, hex.EncodeToString(h.Sum(nil)))
}

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
