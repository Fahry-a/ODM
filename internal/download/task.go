package download

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/storage"
	"odm/internal/transport"
)

// TaskState enumerates the lifecycle states a task moves through. Mirrors the
// PRD §8 color states (queued/dimming, downloading=yellow, retry/error=red,
// complete=green) plus Paused from the RPC API.
type TaskState int

const (
	StateQueued TaskState = iota
	StateActive
	StatePaused
	StateRetrying
	StateCompleted
	StateError

	// persistCheckpointInterval controls how often the control file is flushed
	// during an active download: every N completed chunks the .odm file is
	// rewritten so a crash/kill leaves a usable resume point (aria2-style
	// periodic-persist). persistMinInterval is the wall-clock floor. Writing
	// every chunk was O(n²) for large files — the completed-offset list grows
	// with each chunk, so a 100 GiB file meant re-serialising a ~25k-entry JSON
	// on every chunk — hence the count AND a 2s time gate: a crash loses at
	// worst ~16 chunks / 2 seconds of resume granularity.
	persistCheckpointInterval = 16
	persistMinInterval        = 2 * time.Second

	// resumeVerifyChunks bounds how many completed chunks are sampled and
	// compared against the server when a --continue resume starts, and
	// resumeProbeLen is how many bytes of each sampled chunk are compared.
	resumeVerifyChunks = 4
	resumeProbeLen     = 1024
)

func (s TaskState) String() string {
	switch s {
	case StateQueued:
		return "queued"
	case StateActive:
		return "downloading"
	case StatePaused:
		return "paused"
	case StateRetrying:
		return "retrying"
	case StateCompleted:
		return "completed"
	case StateError:
		return "error"
	}
	return "unknown"
}

// TaskID is the GID-style per-task id (aria2-compatible-ish). surfaced over RPC.
type TaskID string

// ProgressView is a snapshot of a task's live state, safe to copy/reload for
// the UI ticker and the RPC tellStatus handler. All readers go through Snapshot().
type ProgressView struct {
	ID           TaskID
	URL          string
	FinalURL     string
	Filename     string
	State        TaskState
	TotalSize    int64
	BytesDone    int64
	Speed        int64 // bytes/sec, averaged
	Connections  int   // active parallel connections
	ETA          time.Duration
	Errors       int
	Retries      int
	SingleStream bool
}

// Task represents one file's download (PRD §3). A Task owns:
//   - its probe result (size + range-support)
//   - a chunk queue (§11.1)
//   - the pre-allocated destination file + control file
//   - worker goroutines, one per allocated connection
//   - a progress aggregator the UI/RPC poll
type Task struct {
	id      TaskID
	url     string
	opts    TaskOptions
	client  *transport.Client
	lim     *ratelimit.Limiter                // global rate limiter
	taskLim atomic.Pointer[ratelimit.Limiter] // per-task rate limiter; nil = unlimited

	// resolved after Probe
	probe   *transport.ProbeResult
	disk    *storage.File
	queue   *ChunkQueue
	outPath string

	// progress
	bytesDone atomic.Int64
	speed     atomic.Int64
	conns     atomic.Int32
	state     atomic.Int32
	errors    atomic.Int32
	retries   atomic.Int32
	startAt   time.Time

	// periodic control-file checkpoint
	chunksSincePersist int64 // guarded by mu (see checkpoint)
	lastPersist        time.Time
	controlCreatedAt   time.Time // when the control file was first written

	// rate measurement helper (rolling)
	rm   rateMeasure
	rmMu sync.Mutex

	// lifecycle
	cancel context.CancelFunc
	pauseC chan struct{}
	logf   LogFn

	mu     sync.Mutex // guards state transitions & control-file writes
	paused bool

	// mid-flight reallocation
	connTarget atomic.Int32 // desired connection count (≤ conns for drain)
	baseCtx    context.Context
	sink       func(ProgressView)
	workerWg   sync.WaitGroup

	adjustMu   sync.Mutex // guards adjustDone + workerWg.Add race
	adjustDone bool       // true after workerWg.Wait() returns
}

// LogLn is the logger callback signature used by Task: level (info/warn/error)
// plus a printf-style format + args. nil → defaults to a stderr log.Printf.
type LogFn = func(level string, format string, args ...any)

// TaskOptions are the parts of *config.Options a Task needs. Decoupling the
// engine from config keeps download tests buildable from scratch.
type TaskOptions struct {
	OutputName    string // "" ⇒ derive from URL
	Dir           string
	Retry         int
	RetryWait     time.Duration
	Continue      bool
	ChunkSize     int64 // bytes; parsed from --chunk-size
	Timeout       time.Duration
	MaxRedirect   int
	UserAgent     string // for control file metadata
	Checksum      string // "algo:hex" if --checksum was used
	TaskLimitRate string // per-task rate cap, e.g. "2M"; "" = unlimited
}

// rateMeasure keeps a short rolling window of bytes vs time to produce a stable
// instantaneous speed for the UI without flooding (§11.1 "throttled progress").
// We compute bytes/sec over each inter-sample gap and smooth with a light EMA
// so the pacman bar doesn't jitter on tiny buf reads.
type rateMeasure struct {
	lastTS    time.Time
	lastBytes int64
	bps       int64
}

// tick advances the measure by delta bytes and refreshes the EMA'd bytes/sec
// on coarse (≥100ms) intervals so the bar stays smooth. Callers hold rmMu, so
// tick is not concurrent with itself. Returns true when the ~100ms gate elapsed
// and bps was actually recomputed — noteBytes uses this as the progress-feed
// throttle so the UI sink fires on the same cadence (PRD §11.1 "throttled
// progress... every ~100ms") instead of only at chunk completion. Because the
// gate is shared under rmMu across all workers of a task, at most one worker
// crosses the 100ms boundary per window → the sink fires ~10×/s per task, not
// once per worker-per-read (which would flood the snapshot channel).
func (rm *rateMeasure) tick(delta int64) bool {
	rm.lastBytes += delta
	t := time.Now()
	if rm.lastTS.IsZero() {
		rm.lastTS = t
		return false
	}
	elapsed := t.Sub(rm.lastTS)
	if elapsed < 100*time.Millisecond {
		return false
	}
	if elapsed.Seconds() > 0 {
		bps := int64(float64(rm.lastBytes) / elapsed.Seconds())
		if bps > 0 {
			if rm.bps == 0 {
				rm.bps = bps
			} else {
				// light EMA: 60% old / 40% new → smooth but responsive.
				rm.bps = int64(float64(rm.bps)*0.6 + float64(bps)*0.4)
			}
		}
	}
	rm.lastTS = t
	rm.lastBytes = 0
	return true
}

// NewTask constructs a Task (not yet started). Probe + planning happen in Start.
//
// logf is optional; a nil logger becomes a no-op so the hot path (successful
// chunks) never emits. The CLI wires a real leveled logger when --log-level is
// debug/info; default info writes to the chosen --log file.
func NewTask(id TaskID, url string, opts TaskOptions, client *transport.Client, lim *ratelimit.Limiter, logf LogFn) *Task {
	if logf == nil {
		logf = func(level string, format string, args ...any) {}
	}
	taskLim, _ := ratelimit.New(opts.TaskLimitRate)
	t := &Task{
		id:     id,
		url:    url,
		opts:   opts,
		client: client,
		lim:    lim,
		pauseC: make(chan struct{}, 1),
		logf:   logf,
	}
	t.taskLim.Store(taskLim)
	return t
}

// ID returns the task id.
func (t *Task) ID() TaskID { return t.id }

// Snapshot returns a ProgressView safe for UI/RPC use.
func (t *Task) Snapshot() ProgressView {
	v := ProgressView{
		ID:          t.id,
		URL:         t.url,
		Filename:    t.filename(),
		State:       TaskState(t.state.Load()),
		BytesDone:   t.bytesDone.Load(),
		Speed:       t.speed.Load(),
		Connections: int(t.conns.Load()),
		Errors:      int(t.errors.Load()),
		Retries:     int(t.retries.Load()),
	}
	if t.probe != nil {
		v.FinalURL = t.probe.FinalURL
		v.TotalSize = t.probe.TotalSize
		v.SingleStream = t.probe.SingleStream
	}
	v.ETA = t.estimateETA()
	return v
}

func (t *Task) filename() string {
	if t.probe == nil {
		return t.opts.OutputName
	}
	return t.probe.Filename
}

// Filename is the resolved output name (valid after Start's Probe).
func (t *Task) Filename() string { return t.filename() }

// OutputPath is the full destination path.
func (t *Task) OutputPath() string { return t.outPath }

// SupportsRange reports the probe's verdict (valid after Start).
func (t *Task) SupportsRange() bool { return t.probe != nil && t.probe.SupportsRange }

// State reports the current lifecycle state (snapshot through the atomic).
func (t *Task) State() TaskState { return TaskState(t.state.Load()) }

// SetConns overrides the task's connection count. Used by the Scheduler to
// apply the Balancer's per-file allocation, which may differ from the global
// default returned by the TaskMaker.
func (t *Task) SetConns(n int) { t.conns.Store(int32(n)); t.connTarget.Store(int32(n)) }

// AdjustConns changes the desired connection count at runtime. When target is
// lower than the current count, excess workers gracefully drain after finishing
// their current chunk (no mid-chunk cancels). When target is higher, additional
// worker goroutines are spawned. Safe to call concurrently with Start.
// Returns true if the adjustment was applied, false if the task has already
// finished (no workers can be spawned).
func (t *Task) AdjustConns(target int, ctx context.Context, sink func(ProgressView)) bool {
	if ctx == nil {
		ctx = t.baseCtx
	}
	if sink == nil {
		sink = t.sink
	}
	old := t.connTarget.Swap(int32(target))
	// If target < old, workers self-terminate via the graceful drain check at
	// the top of their loop — no further action needed for reduction.
	if target <= int(old) {
		return true
	}

	// Increase path: guard against the race where workerWg.Wait() has already
	// returned in Start. Adding to a WaitGroup after Wait returns is undefined
	// behaviour in Go.
	t.adjustMu.Lock()
	if t.adjustDone {
		t.adjustMu.Unlock()
		return false
	}
	for i := 0; i < target-int(old); i++ {
		t.workerWg.Add(1)
		go t.worker(ctx, -1, &t.workerWg, sink)
	}
	t.adjustMu.Unlock()
	return true
}

// Start runs Probe → open file → start workers. Blocks until the task finishes
// (completed or errored) or ctx is cancelled. progressSink receives periodic
// snapshots for the UI/RPC aggregator; pass nil to opt out.
func (t *Task) Start(ctx context.Context, conns int, progressSink func(ProgressView)) error {
	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	defer cancel()
	t.startAt = time.Now()
	t.setState(StateActive)
	t.baseCtx = ctx
	t.sink = progressSink

	// 1. Probe.
	t.logf("info", "probing %s", t.url)
	pr, err := t.client.Probe(ctx, t.url)
	if err != nil {
		t.setState(StateError)
		t.emitFinal(progressSink)
		return fmt.Errorf("probe: %w", err)
	}
	t.probe = pr
	if pr.Filename == "" {
		pr.Filename = deriveFilename(pr.FinalURL, t.opts.OutputName)
	} else if t.opts.OutputName != "" {
		pr.Filename = t.opts.OutputName // explicit -o wins
	}
	t.setState(StateActive)

	// 2. Resolve paths + attempt resume.
	dir := t.opts.Dir
	outName := pr.Filename
	if outName == "" {
		outName = "download.bin"
	}
	t.outPath = filepath.Join(dir, outName)

	// Probe-derived size check before opening. A SingleStream verdict means the
	// server won't honour ranged GETs, so the queue MUST hold exactly one
	// whole-file chunk regardless of the known size — otherwise worker N would
	// pull chunk N, the server answers the ranged request with the full body,
	// and that body gets written at the chunk's offset, corrupting the file
	// (and over-counting bytesDone past TotalSize, which used to let the task
	// "succeed" on the corrupt output). NewChunkQueue's totalSize<0 branch is
	// exactly the single whole-file chunk layout we want.
	qs := pr.TotalSize
	if pr.SingleStream {
		qs = -1
	}
	q := NewChunkQueue(qs, t.opts.ChunkSize)

	alreadyDone := int64(0)
	if t.opts.Continue {
		if cf, cerr := storage.LoadControl(t.outPath); cerr == nil {
			// ETag validation: if both are non-empty and don't match, the file
			// changed on the server — do NOT resume stale chunks.
			if cf.ETag != "" && pr.ETag != "" && cf.ETag != pr.ETag {
				t.logf("warn", "ETag changed (%s → %s), re-downloading from scratch", cf.ETag, pr.ETag)
			} else if cf.TotalSize == pr.TotalSize && cf.ChunkSize == t.opts.ChunkSize {
				var ok bool
				alreadyDone, ok = q.ResetCompletedOffsets(cf.CompletedOffsets(), pr.TotalSize)
				if !ok {
					// e.g. a ranged control file now hitting a single-stream URL,
					// or a stale layout: trust nothing, start over.
					t.logf("warn", "control file layout doesn't match this download, re-downloading from scratch")
					alreadyDone = 0
				} else {
					t.bytesDone.Store(alreadyDone)
					t.logf("info", "resuming %s: %d bytes already written", outName, alreadyDone)
				}
			}
		}
	}

	disk, err := storage.OpenForWrite(dir, outName, pr.TotalSize)
	if err != nil {
		t.setState(StateError)
		t.emitFinal(progressSink)
		return err
	}
	t.disk = disk
	defer disk.Close()
	t.queue = q

	if alreadyDone > 0 {
		// Resume integrity check: spot-check completed chunks against the server
		// so a silently-changed file (no ETag to detect it) or corrupt/stale
		// on-disk bytes can't be resumed into a corrupt result. Any mismatch →
		// full re-download.
		if err := t.verifyResumedChunks(ctx); err != nil {
			t.logf("warn", "resume integrity check failed (%v) — re-downloading from scratch", err)
			alreadyDone = 0
			q = NewChunkQueue(qs, t.opts.ChunkSize)
			t.queue = q
		}
	}

	if pr.TotalSize >= 0 && alreadyDone >= pr.TotalSize && pr.TotalSize > 0 {
		// Already complete on disk (resume found everything done).
		t.bytesDone.Store(pr.TotalSize)
		t.setState(StateCompleted)
		if err := t.verifyChecksum(); err != nil {
			t.logf("error", "checksum: %v", err)
			t.setState(StateError)
			t.emitFinal(progressSink)
			return err
		}
		t.emitFinal(progressSink)
		return t.finish()
	}

	// 3. Launch workers.
	t.conns.Store(int32(conns))
	t.connTarget.Store(int32(conns))
	if pr.TotalSize <= 0 && pr.SingleStream {
		// Single-stream fallback: exactly one worker on the single whole-file chunk.
		conns = 1
		t.conns.Store(1)
		t.connTarget.Store(1)
	}

	// Write the control file immediately so it's visible on disk from the
	// start (like aria2's .aria2), not only after the first checkpoint.
	t.persistControl()

	for i := range conns {
		t.workerWg.Add(1)
		go t.worker(ctx, i, &t.workerWg, progressSink)
	}
	// progress ticker even in single-worker sizeless case.
	t.workerWg.Wait()
	t.adjustMu.Lock()
	t.adjustDone = true
	t.adjustMu.Unlock()

	if t.errors.Load() > 0 {
		t.setState(StateError)
		// Persist completed chunks before exiting so partial progress
		// survives even if the process terminates shortly after this
		// returns (e.g. via os.Exit on signal).
		t.persistControl()
		t.emitFinal(progressSink)
		return fmt.Errorf("task failed: %d chunk errors, %d/%d bytes", t.errors.Load(), t.bytesDone.Load(), t.totalOrDone())
	}
	t.setState(StateCompleted)
	// Checksum verification runs against the actual written file. A mismatch
	// fails the task even though the transfer itself finished.
	if err := t.verifyChecksum(); err != nil {
		t.logf("error", "checksum: %v", err)
		t.setState(StateError)
		t.persistControl()
		t.emitFinal(progressSink)
		return err
	}
	t.emitFinal(progressSink)
	return t.finish()
}

func (t *Task) setState(s TaskState) { t.state.Store(int32(s)) }

// worker pulls chunks from the shared queue and downloads them with retry on
// transient failures (§13). Each chunk write uses storage.WriteAt so offset
// positioning is safe without locks.
func (t *Task) worker(ctx context.Context, _ int, wg *sync.WaitGroup, sink func(ProgressView)) {
	defer wg.Done()
	// Each worker represents one live connection. When it retires (queue empty,
	// chunk error, or ctx cancel), decrement the live-connection counter so the
	// UI's [xN] reflects the actual number of connections still downloading,
	// not the number originally launched. Guard against underflow.
	defer func() {
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
		// Graceful drain: if we have more live workers than the target,
		// this worker retires before pulling the next chunk. Connections
		// are decremented in the defer, not here, so the count drops
		// naturally as workers exit.
		if t.conns.Load() > t.connTarget.Load() {
			return
		}

		if t.isPaused() {
			select {
			case <-ctx.Done():
				return
			case <-t.unpauseSignal():
			}
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		c, ok := t.queue.Next()
		if !ok {
			return // no more work
		}

		if err := t.downloadChunk(ctx, c, sink); err != nil {
			if ctx.Err() != nil {
				t.errors.Add(1)
				// Persist completed chunks so partial progress survives
				// even if the process exits before Start's error path runs.
				t.persistControl()
				return
			}
			// Requeue the chunk so another worker gives it a fresh chance
			// instead of failing the whole task on one transient worker
			// failure. The chunk is dropped only after opts.Retry worker-level
			// passes (each pass running its own internal retry budget); beyond
			// that it's treated as permanently broken.
			if t.queue.Requeue(c, t.opts.Retry) {
				t.logf("warn", "chunk %d (off %d) failed, requeued for retry: %v", c.Index, c.Start, err)
				continue
			}
			t.errors.Add(1)
			t.setState(StateError)
			t.logf("error", "chunk %d (off %d) failed permanently: %v", c.Index, c.Start, err)
			return
		}
		t.queue.MarkDone(c, t.probe.TotalSize)
		if sink != nil {
			sink(t.getCurrent(c))
		}
		// Periodic checkpoint: flush the control file on the count/time gate so
		// a crash/kill leaves a usable resume point (similar to aria2's periodic
		// .aria2 persist) without re-serialising the whole file every chunk.
		if t.checkpoint() {
			t.persistControl()
		}
	}
}

// downloadChunk fetches one chunk's byte-range (retrying up to opts.Retry times
// with RetryWait backoff) and writes it to disk at the chunk's offset.
func (t *Task) downloadChunk(ctx context.Context, c Chunk, sink func(ProgressView)) error {
	var lastErr error
	for attempt := range t.opts.Retry + 1 {
		if attempt > 0 {
			t.setState(StateRetrying)
			t.retries.Add(1)
			wait := time.Duration(attempt) * t.opts.RetryWait
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		t.setState(StateActive)
		err := t.fetchAndWrite(ctx, c, sink)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		t.logf("warn", "chunk %d attempt %d: %v", c.Index, attempt, err)
	}
	return lastErr
}

// fetchAndWrite does a single ranged GET and copies the body to disk, throttled
// by the global limiter and accounting bytes into progress.
func (t *Task) fetchAndWrite(ctx context.Context, c Chunk, sink func(ProgressView)) error {
	// Per-chunk timeout prevents a stalled connection from hanging a worker
	// forever. Each chunk gets Timeout*10 to complete (default 300s = 5 min).
	// If the timeout fires, the chunk is retried by the caller (downloadChunk).
	chunkTimeout := t.opts.Timeout * 10
	if chunkTimeout <= 0 {
		chunkTimeout = 300 * time.Second
	}
	chunkCtx, chunkCancel := context.WithTimeout(ctx, chunkTimeout)
	defer chunkCancel()

	// Sizeless single-stream chunk: plain GET, no Range.
	if t.probe.TotalSize < 0 || (t.probe.SingleStream && c.Start == 0 && c.Index == 0) {
		return t.fetchWhole(chunkCtx, c, sink)
	}

	end := c.End
	if end < 0 {
		end = -1
	}
	rr, err := t.client.GetRange(chunkCtx, t.probe.FinalURL, c.Start, end)
	if err != nil {
		return err
	}
	defer rr.Resp.Body.Close()

	body := rr.Resp.Body
	if !t.lim.Unlimited() {
		body = io.NopCloser(t.lim.Reader(chunkCtx, body))
	}
	if tl := t.taskLim.Load(); tl != nil && !tl.Unlimited() {
		body = io.NopCloser(tl.Reader(chunkCtx, body))
	}

	// Copy chunk to disk at offset Start using a small buffer; the WriteAt
	// positions the write at Start regardless of the file pointer.
	buf := make([]byte, 64*1024)
	var off int64
	n, err := copyChunkFrom(body, t.disk, c.Start, buf, &off, func(delta int64) {
		t.noteBytes(delta, sink)
	})
	if err != nil {
		return err
	}
	// Validate we got the expected chunk size when it's bounded.
	if c.End >= 0 && t.probe.TotalSize > 0 && off != (c.End-c.Start+1) {
		return fmt.Errorf("chunk %d short read: got %d want %d", c.Index, off, c.End-c.Start+1)
	}
	_ = n
	return nil
}

// fetchWhole handles the sizeless or range-less single-stream case (§11.2):
// plain GET of the whole resource, sequential write at offset 0; bytes are
// counted into progress but the total stays -1 so the UI shows "sizeless".
func (t *Task) fetchWhole(ctx context.Context, _ Chunk, sink func(ProgressView)) error {
	// Per-chunk timeout prevents a stalled connection from hanging forever.
	chunkTimeout := t.opts.Timeout * 10
	if chunkTimeout <= 0 {
		chunkTimeout = 300 * time.Second
	}
	chunkCtx, chunkCancel := context.WithTimeout(ctx, chunkTimeout)
	defer chunkCancel()

	req, err := t.client.NewGetRequest(chunkCtx, t.probe.FinalURL)
	if err != nil {
		return err
	}
	resp, err := t.client.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := resp.Body
	if !t.lim.Unlimited() {
		body = io.NopCloser(t.lim.Reader(chunkCtx, body))
	}
	if tl := t.taskLim.Load(); tl != nil && !tl.Unlimited() {
		body = io.NopCloser(tl.Reader(chunkCtx, body))
	}
	buf := make([]byte, 64*1024)
	_, err = copyChunkFrom(body, t.disk, 0, buf, new(int64), func(delta int64) {
		t.noteBytes(delta, sink)
	})
	return err
}

// copyChunkFrom copies r into w.WriteAt at base offset, advancing a local
// offset counter, calling onProgress for each read's delta. Returns total n.
func copyChunkFrom(r io.Reader, w *storage.File, base int64, buf []byte, off *int64, onProgress func(int64)) (int64, error) {
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.WriteAt(buf[:n], base+*off); werr != nil {
				return total, werr
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
// (PRD §11.1 "throttled progress"). sink may be nil (the Manager.Run test path
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

// getCurrent snapshots progress after a chunk.
func (t *Task) getCurrent(_ Chunk) ProgressView { return t.Snapshot() }

// emitFinal pushes one last snapshot carrying the terminal state (Completed
// or Error) through the progressSink before Start returns. This is the root
// cause fix for the §3.1 "Total: 0/0 / completed line vanishes" bug: the
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
func (t *Task) emitFinal(sink func(ProgressView)) {
	if sink == nil {
		return
	}
	sink(t.Snapshot())
}

// estimateETA: bytes-left / speed. The inner expression produces nanoseconds
// directly (remaining×1e9 / bytes-per-sec); converting to time.Duration yields
// the correct duration. There is intentionally NO trailing * time.Second —
// the previous code multiplied by time.Second a second time, inflating the
// result by 1e9 and capping every ETA to 99:59:59.
func (t *Task) estimateETA() time.Duration {
	if t.probe == nil || t.probe.TotalSize <= 0 {
		return 0
	}
	done := t.bytesDone.Load()
	if done >= t.probe.TotalSize {
		return 0
	}
	sp := t.speed.Load()
	if sp <= 0 {
		return 0
	}
	eta := time.Duration((t.probe.TotalSize - done) * int64(time.Second) / sp)
	if eta < 0 {
		return 0
	}
	return eta
}

// totalOrDone is the total size, or bytes done if total unknown.
func (t *Task) totalOrDone() int64 {
	if t.probe != nil && t.probe.TotalSize > 0 {
		return t.probe.TotalSize
	}
	return t.bytesDone.Load()
}

// verifyChecksum runs the --checksum verification against the real output file
// (t.outPath — the name the server chose via Content-Disposition or the -o
// override, not a URL-derived guess). No-op when no checksum was requested.
func (t *Task) verifyChecksum() error {
	if t.opts.Checksum == "" {
		return nil
	}
	algo, hexStr, ok := strings.Cut(t.opts.Checksum, ":")
	if !ok || hexStr == "" {
		return fmt.Errorf("checksum: bad spec %q", t.opts.Checksum)
	}
	return verifyChecksum(t.outPath, algo, hexStr)
}

// finish flushes and persist/removes the control file; an error from the
// caller already set the state.
func (t *Task) finish() error {
	if t.disk != nil {
		_ = t.disk.Sync()
	}
	// On success the control file is removed (§12 step 8).
	if TaskState(t.state.Load()) == StateCompleted {
		_ = storage.RemoveControl(t.outPath)
	} else {
		// Persist what we have so a later --continue can resume.
		t.persistControl()
	}
	return nil
}

// checkpoint returns true when the control file should be flushed: either
// persistCheckpointInterval chunks have completed since the last flush, or
// persistMinInterval has elapsed since the last write. Serialized under t.mu
// (the same lock persistControl takes) so the counters are race-free across
// concurrent workers.
func (t *Task) checkpoint() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chunksSincePersist++
	if t.chunksSincePersist >= persistCheckpointInterval {
		t.chunksSincePersist = 0
		return true
	}
	return !t.lastPersist.IsZero() && time.Since(t.lastPersist) >= persistMinInterval
}

// persistControl records completed chunk offsets so resume picks up. Mutex
// guarded: several workers (and the error/cancel paths) can call it
// concurrently, and SaveControl writes a shared temp path — concurrent writes
// would interleave and risk a corrupt .odm file.
func (t *Task) persistControl() {
	if t.probe == nil || t.queue == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.controlCreatedAt.IsZero() {
		t.controlCreatedAt = now
	}
	t.lastPersist = now
	cf := &storage.ControlFile{
		URL:       t.url,
		FinalURL:  t.probe.FinalURL,
		TotalSize: t.probe.TotalSize,
		ChunkSize: t.opts.ChunkSize,
		ETag:      t.probe.ETag,
		Completed: t.queue.completedOffsetsLocked(),
		// Extended metadata
		CreatedAt:   t.controlCreatedAt,
		UpdatedAt:   now,
		Connections: int(t.conns.Load()),
		UserAgent:   t.opts.UserAgent,
		ODMVersion:  Version,
		Checksum:    t.opts.Checksum,
	}
	_ = storage.SaveControl(t.outPath, cf)
}

// verifyResumedChunks samples a handful of the chunks the control file claims
// are already on disk and compares them against the server with ranged GETs. A
// mismatch means the file changed server-side (with no ETag to detect it) or
// the local copy is stale/corrupt, so the caller re-downloads from scratch.
// Single-stream downloads are skipped: the single whole-file chunk can't be
// sampled without effectively re-downloading it, and its resume is guarded by
// the ETag/size checks. Fails safe — any request/read error is treated as a
// mismatch.
func (t *Task) verifyResumedChunks(ctx context.Context) error {
	if t.probe == nil || t.probe.SingleStream || t.probe.TotalSize <= 0 {
		return nil
	}
	spans := t.queue.CompletedSpans(t.opts.ChunkSize, t.probe.TotalSize, resumeVerifyChunks)
	for _, s := range spans {
		end := s.Start + resumeProbeLen - 1
		if end > s.End {
			end = s.End
		}
		want := int(end - s.Start + 1)
		chkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		rr, err := t.client.GetRange(chkCtx, t.probe.FinalURL, s.Start, end)
		if err != nil {
			cancel()
			return fmt.Errorf("resume check at %d: %w", s.Start, err)
		}
		got := make([]byte, want)
		_, rerr := io.ReadFull(rr.Resp.Body, got)
		rr.Resp.Body.Close()
		cancel()
		if rerr != nil {
			return fmt.Errorf("resume check at %d: %v", s.Start, rerr)
		}
		disk := make([]byte, want)
		if _, derr := t.disk.ReadAt(disk, s.Start); derr != nil {
			return fmt.Errorf("resume check at %d: %v", s.Start, derr)
		}
		if !bytes.Equal(got, disk) {
			return fmt.Errorf("resume check at %d: on-disk data differs from server", s.Start)
		}
	}
	return nil
}

// SetTaskRate updates the per-task rate limit at runtime. spec="" or "off" →
// unlimited. Used by RPC changeOption with "max-download-limit-per-task". Safe
// for concurrent use: creates a new limiter atomically (readers snapshot
// t.taskLim when wrapping the body, so an in-flight read finishes with the old
// value; subsequent reads pick up the new one).
func (t *Task) SetTaskRate(spec string) bool {
	l, err := ratelimit.New(spec)
	if err != nil {
		return false
	}
	t.taskLim.Store(l)
	return true
}

// Pause / Unpause are RPC-facing hooks (§10. pause/unpause).
func (t *Task) Pause() {
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	t.setState(StatePaused)
}
func (t *Task) Unpause() {
	t.mu.Lock()
	t.paused = false
	t.mu.Unlock()
	select {
	case t.pauseC <- struct{}{}:
	default:
	}
	t.setState(StateActive)
}
func (t *Task) Cancel() {
	if t.cancel != nil {
		t.cancel()
	}
}
func (t *Task) isPaused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}
func (t *Task) unpauseSignal() <-chan struct{} { return t.pauseC }

// ListCompletedOffsetsLocked is used by persistControl; name says locked but
// the chunkqueue method is itself locked, this just bridges naming.
func (q *ChunkQueue) completedOffsetsLocked() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, 0, len(q.completed))
	for k := range q.completed {
		out = append(out, k)
	}
	return out
}

// deriveFilename picks an output name from the URL path, or falls back to the
// --output override / "download.bin".
func deriveFilename(finalURL, override string) string {
	if override != "" {
		return override
	}
	u := finalURL
	if i := strings.LastIndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		name := u[i+1:]
		if name != "" {
			return name
		}
	}
	return "download.bin"
}
