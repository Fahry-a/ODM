package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/storage"
	"odm/internal/transport"
	"odm/internal/version"
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

	// resolved after Probe. Held as an atomic pointer because the RPC daemon
	// admits a task the moment NewTask returns — a client can poll tellStatus
	// via the WS server while Start is still probing, so readers (Snapshot,
	// filename, persistControl, verify*) must not race the writer.
	probe   atomic.Pointer[transport.ProbeResult]
	disk    *storage.File
	queue   workQueue
	outPath string

	// ariaSplit is the effective segment count computed for the aria2c
	// profile (0 for odm). Used to cap workers at the segment count and to
	// rebuild the layout on resume.
	ariaSplit int64

	// both profile: engines[0] covers [0, splitAt) on the h1 client,
	// engines[1] covers [splitAt, end) on the h2 client. nil for other
	// profiles. regionConns holds the worker count per engine.
	engines     []*Engine
	regionConns []int
	splitAt     int64

	// h2Client is the HTTP/2 transport client (region2 of both; the same as
	// t.client for aria2c). Set by the Manager via SetH2Client.
	h2Client *transport.Client

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
	cancel    context.CancelFunc
	cancelled atomic.Bool // set by Cancel; Start fails fast if cancelled while queued
	pauseC    chan struct{}
	logf      LogFn

	mu     sync.Mutex // guards state transitions, control-file writes, and chunkHashes
	paused bool

	// chunkHashes records the SHA-256 of each successfully completed chunk's
	// bytes, keyed by the chunk's Start offset. Guarded by mu (store happens
	// once per completed chunk, never on the copy hot loop). Checkpoints
	// persist these alongside CompletedOffsets so a resume can verify all
	// completed chunks against local disk.
	chunkHashes map[int64]string

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
	ChunkSize     int64 // bytes; parsed from --chunk-size (fixed chunk for odm, min for aria2c)
	Timeout       time.Duration
	UserAgent     string // for control file metadata
	Checksum      string // "algo:hex" if --checksum was used
	TaskLimitRate string // per-task rate cap, e.g. "2M"; "" = unlimited

	Profile          string // engine profile: odm|aria2c|both|smart ("" = odm)
	Split            int    // aria2c: --split (segments count), default 5
	MinSplitSize     int64  // aria2c: --min-split-size, default 20 MiB
	MaxConnPerServer int    // aria2c: -x (per-server connection cap, default 1)
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
				// EMA: 30% old / 70% new → responsive to real speed changes.
				// Previous 60/40 caused stale speed display for 1-2s after bursts.
				rm.bps = int64(float64(rm.bps)*0.3 + float64(bps)*0.7)
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
		id:          id,
		url:         url,
		opts:        opts,
		client:      client,
		lim:         lim,
		pauseC:      make(chan struct{}),
		chunkHashes: map[int64]string{},
		logf:        logf,
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
	if pr := t.probe.Load(); pr != nil {
		v.FinalURL = pr.FinalURL
		v.TotalSize = pr.TotalSize
		v.SingleStream = pr.SingleStream
	}
	v.ETA = t.estimateETA()
	return v
}

func (t *Task) filename() string {
	if pr := t.probe.Load(); pr != nil {
		return pr.Filename
	}
	return t.opts.OutputName
}

// OutputPath is the full destination path.
func (t *Task) OutputPath() string { return t.outPath }

// State reports the current lifecycle state (snapshot through the atomic).
func (t *Task) State() TaskState { return TaskState(t.state.Load()) }

// SetConns overrides the task's connection count. Used by the Scheduler to
// apply the Balancer's per-file allocation, which may differ from the global
// default returned by the TaskMaker.
func (t *Task) SetConns(n int) { t.conns.Store(int32(n)); t.connTarget.Store(int32(n)) }

// SetH2Client attaches the HTTP/2 transport client used for the both
// profile's region2 (and re-probe). Must be called before Start.
func (t *Task) SetH2Client(c *transport.Client) {
	if c != nil {
		t.h2Client = c
	}
}

// SetProbe attaches a probe to a task so Start can skip the network probe.
// The CLI one-shot path probes every URL up front (for the Balancer and the
// confirmation prompt) and injects it here; the RPC daemon path leaves it nil
// and Start probes normally. Must be called before Start.
func (t *Task) SetProbe(pr *transport.ProbeResult) {
	if pr != nil {
		t.probe.Store(pr)
	}
}

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
		go t.worker(ctx, t.currentEngine(), &t.workerWg, sink)
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
	// A task removed via RPC while still queued never reached Start before;
	// Cancel had no ctx to cancel. If it was cancelled, fail fast instead of
	// downloading a file the caller no longer wants.
	if t.cancelled.Load() {
		cancel()
		t.setState(StateError)
		t.emitFinal(progressSink)
		return fmt.Errorf("task cancelled before start")
	}
	t.startAt = time.Now()
	t.setState(StateActive)
	t.baseCtx = ctx
	t.sink = progressSink

	// 1. Probe. The CLI one-shot path already probed every URL for the Balancer
	// and confirmation prompt and injects the result via SetProbe, so a fresh
	// network probe (HEAD + ranged GET) is skipped there. The RPC daemon path
	// leaves t.probe nil and probes here as usual.
	pr := t.probe.Load()
	if pr == nil {
		t.logf("info", "probing %s", t.url)
		var perr error
		pr, perr = t.client.Probe(ctx, t.url)
		if perr != nil {
			t.setState(StateError)
			t.emitFinal(progressSink)
			return fmt.Errorf("probe: %w", perr)
		}
		t.probe.Store(pr)
	}
	// Smart profile: decide the concrete engine now that the probe answered
	// range support + size, and check h2 readiness through the h2 client.
	// The CLI path already resolved smart to a concrete profile in the
	// TaskMaker (it has the h2 probe pass); here we only handle the RPC
	// daemon path where Start probes lazily.
	if t.opts.Profile == "smart" {
		profile, reason := ChooseProfile(ServerCapabilities{
			TotalSize:     pr.TotalSize,
			SupportsRange: pr.SupportsRange,
			SingleStream:  pr.SingleStream,
			HTTP2Ready:    t.h2Client != nil && t.h2Client.SupportsHTTP2(ctx, t.url),
			Conns:         conns,
		})
		t.logf("info", "smart profile: chose %q (%s)", profile, reason)
		t.opts.Profile = profile
	}
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
	// Engine profile: aria2c splits the file into `split` segments of roughly
	// equal size (bounded by min-split-size), odm uses fixed-size chunks with
	// work-stealing. both splits the file into two regions — [0, mid) via the
	// odm engine (h1 client), [mid, end) via the aria2c engine (h2 client).
	// The layout is deterministic from (TotalSize, split params), so resume
	// rebuilds it from the control file.
	effective := t.opts.ChunkSize
	isAria := t.opts.Profile == "aria2c"
	isBoth := t.opts.Profile == "both"
	t.engines = nil
	t.splitAt = 0
	if isAria && qs > 0 {
		n, seg := AriaSplit(qs, int64(t.opts.Split), t.opts.MinSplitSize)
		effective = seg
		t.ariaSplit = n
		t.logf("info", "aria2c profile: %d segments of ~%s each", n, formatSegSize(seg))
	}
	if isBoth && qs > 0 && qs < 4*1024*1024 {
		// Tiny file: a 50/50 split gains nothing (the aria2c region would be
		// a couple of segments at most). Degrade to the plain odm engine.
		isBoth = false
		t.logf("info", "both profile: file < 4 MiB, using single-region odm engine")
	}
	if isBoth && qs > 0 {
		// both: region1 [0, mid) = odm fixed-chunk work-stealing (h1 client),
		// region2 [mid, end) = aria2c static split (h2 client). Connection
		// budget halves; a single connection or tiny file degrades to the odm
		// engine (see below).
		conns1 := max(1, conns/2)
		conns2 := max(1, conns-conns1)
		t.regionConns = []int{conns1, conns2}
		t.splitAt = qs / 2
		if t.splitAt < 1 {
			t.splitAt = 1
		}
		mid := t.splitAt
		n2, _ := AriaSplit(qs-mid, int64(t.opts.Split), t.opts.MinSplitSize)
		eng2Client := t.client
		if t.h2Client != nil {
			eng2Client = t.h2Client
		}
		t.engines = []*Engine{
			{q: NewChunkQueue(mid, t.opts.ChunkSize), client: t.client, base: 0},
			{q: NewStaticQueue(qs-mid, n2), client: eng2Client, base: mid},
		}
		t.ariaSplit = n2
		t.logf("info", "both profile: region1 [0,%d) odm (%d conns, h1), region2 [%d,%d) aria2c (%d segments, h2)",
			mid, conns1, mid, qs, n2)
	}
	var q workQueue
	if t.engines != nil {
		q = t.engines[0].q
	} else if isAria && qs > 0 {
		q = NewStaticQueue(qs, t.ariaSplit)
	} else {
		q = NewChunkQueue(qs, effective)
	}
	t.queue = q // set early: resume restore/verification below reads the queue

	alreadyDone := int64(0)
	var controlFile *storage.ControlFile
	if t.opts.Continue {
		if cf, cerr := storage.LoadControl(t.outPath); cerr == nil {
			controlFile = cf
			// ETag validation: if both are non-empty and don't match, the file
			// changed on the server — do NOT resume stale chunks.
			if cf.ETag != "" && pr.ETag != "" && cf.ETag != pr.ETag {
				t.logf("warn", "ETag changed (%s → %s), re-downloading from scratch", cf.ETag, pr.ETag)
			} else if cf.TotalSize == pr.TotalSize &&
				cf.ChunkSize == effective &&
				(cf.Profile == "" || cf.Profile == t.opts.Profile) &&
				cf.SplitAt == t.splitAt &&
				cf.Region2ChunkSize == t.region2ChunkSize() {
				var ok bool
				offs := cf.CompletedOffsets()
				if t.engines != nil {
					// Split the absolute offsets per region: region1 keeps them,
					// region2 subtracts its base (the queue is 0-based there).
					done1 := map[int64]struct{}{}
					done2 := map[int64]struct{}{}
					for off := range offs {
						if off >= t.splitAt {
							done2[off-t.splitAt] = struct{}{}
						} else {
							done1[off] = struct{}{}
						}
					}
					var a1, a2 int64
					a1, ok = t.engines[0].ResetCompletedOffsets(done1, t.splitAt)
					a2, _ = t.engines[1].ResetCompletedOffsets(done2, t.engines[1].base)
					alreadyDone = a1 + a2
				} else {
					alreadyDone, ok = q.ResetCompletedOffsets(offs, pr.TotalSize)
				}
				if !ok {
					// e.g. a ranged control file now hitting a single-stream URL,
					// or a stale layout. Trust nothing, re over.
					t.logf("warn", "control file layout doesn't match this download, re-downloading from scratch")
					alreadyDone = 0
				} else {
					t.bytesDone.Store(alreadyDone)
					// Carry the recorded hashes into this run so checkpoints keep
					// persisting them (otherwise the next resume would silently
					// downgrade to the legacy server-compare fallback).
					t.restoreChunkHashes(cf)
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

	if alreadyDone > 0 {
		// Resume integrity check: verify the completed chunks are intact before
		// trusting them. Two complementary checks — per-chunk SHA-256 hashes
		// verify every completed chunk against local disk (catches local
		// corruption), and the sampled server-side compare detects server drift
		// (a same-size replacement the ETag check can't see). Legacy control
		// files (no hashes) rely on the server compare alone. Any mismatch →
		// full re-download.
		if err := t.verifyResumedData(ctx, controlFile); err != nil {
			t.logf("warn", "resume integrity check failed (%v) — re-downloading from scratch", err)
			alreadyDone = 0
			t.bytesDone.Store(0)
			t.clearChunkHashes()
			if t.engines != nil {
				// Rebuild both engines with the same layout math.
				mid := t.splitAt
				n2, _ := AriaSplit(qs-mid, int64(t.opts.Split), t.opts.MinSplitSize)
				eng2Client := t.client
				if t.h2Client != nil {
					eng2Client = t.h2Client
				}
				t.engines = []*Engine{
					{q: NewChunkQueue(mid, t.opts.ChunkSize), client: t.client, base: 0},
					{q: NewStaticQueue(qs-mid, n2), client: eng2Client, base: mid},
				}
				q = t.engines[0].q
			} else if isAria && qs > 0 {
				q = NewStaticQueue(qs, t.ariaSplit)
			} else {
				q = NewChunkQueue(qs, effective)
			}
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

	// aria2c profile: cap the concurrent workers at the effective split count
	// (fewer segments than conns means some conns idle — aria2c behaves the
	// same) and at MaxConnPerServer for h1 (each stream is a separate TCP
	// connection there). For h2 the -x cap is irrelevant: all streams share
	// one connection.
	workerCount := conns
	if isAria {
		if t.ariaSplit > 0 && int64(workerCount) > t.ariaSplit {
			workerCount = int(t.ariaSplit)
		}
		if pr.TotalSize > 0 && !t.profileUsesH2() && t.opts.MaxConnPerServer > 0 && workerCount > t.opts.MaxConnPerServer {
			workerCount = t.opts.MaxConnPerServer
		}
	}
	if t.engines != nil {
		// both profile: spawn per-region workers with the region's conns.
		for ei, eng := range t.engines {
			n := t.regionConns[ei]
			for i := 0; i < n; i++ {
				t.workerWg.Add(1)
				go t.worker(ctx, eng, &t.workerWg, progressSink)
			}
		}
	} else {
		engine := t.currentEngine()
		for range workerCount {
			t.workerWg.Add(1)
			go t.worker(ctx, engine, &t.workerWg, progressSink)
		}
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

// profileUsesH2 reports whether the task's profile speaks HTTP/2 (aria2c and
// both-region2). Used to decide whether the -x per-server cap applies: under
// h2 all streams share one TCP connection, so the cap is meaningless there.
func (t *Task) profileUsesH2() bool {
	return t.opts.Profile == "aria2c" || t.opts.Profile == "both"
}

// currentEngine returns the engine a single-profile task's workers use (the
// shared one, or the first region for both — both paths pick explicitly).
func (t *Task) currentEngine() *Engine {
	if t.engines != nil {
		return t.engines[0]
	}
	return &Engine{q: t.queue, client: t.client, base: 0}
}

// engineForStart resolves which engine owns an absolute chunk start: region2
// in the both profile, the shared engine otherwise.
func (t *Task) engineForStart(start int64) *Engine {
	if t.engines != nil {
		// Chunk starts are absolute in both engines (region2 records base+rel
		// offsets), so the splitAt check is on the absolute value.
		if start >= t.splitAt {
			return t.engines[1]
		}
		return t.engines[0]
	}
	return t.currentEngine()
}

// formatSegSize renders a segment byte count compactly for log lines
// (MiB/GiB…). Kept local — the UI package's formatter lives in internal/ui.
func formatSegSize(b int64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	val := float64(b)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	idx := -1
	for val >= unit && idx < len(units)-1 {
		val /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", val, units[idx])
}

// worker pulls chunks from the given engine's queue and downloads them with
// retry on transient failures (§13). Each chunk write uses storage.WriteAt so
// offset positioning is safe without locks. In the both profile each region
// has its own engine; single-profile tasks pass the one shared engine.
func (t *Task) worker(ctx context.Context, eng *Engine, wg *sync.WaitGroup, sink func(ProgressView)) {
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

// downloadChunk fetches one chunk's byte-range (retrying up to opts.Retry times
// with RetryWait backoff) and writes it to disk at the chunk's offset. `eng`
// supplies the region base (both profile) and transport client.
func (t *Task) downloadChunk(ctx context.Context, eng *Engine, c Chunk, sink func(ProgressView)) error {
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
		// Per-attempt hasher: every byte written to disk is fed through it, but
		// the digest is only recorded on a fully-successful attempt — a hash is
		// never stored for a partially-written chunk.
		h := sha256.New()
		err := t.fetchAndWrite(ctx, eng, c, sink, h)
		if err == nil {
			t.storeChunkHash(eng.AbsStart(c.Start), hex.EncodeToString(h.Sum(nil)))
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

// chunkTimeoutCtx derives the per-chunk context: Timeout*10 (default 300s)
// so a stalled connection can't hang a worker forever.
func (t *Task) chunkTimeoutCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	tmo := t.opts.Timeout * 10
	if tmo <= 0 {
		tmo = 300 * time.Second
	}
	return context.WithTimeout(ctx, tmo)
}

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
	resp, err := eng.Client().GetRange(chunkCtx, pr.FinalURL, absStart, absEnd)
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
	// Validate we got the expected chunk size when it's bounded.
	if c.End >= 0 && pr.TotalSize > 0 && off != (c.End-c.Start+1) {
		return fmt.Errorf("chunk %d short read: got %d want %d", c.Index, off, c.End-c.Start+1)
	}
	_ = n
	return nil
}

// fetchWhole handles the sizeless or range-less single-stream case (§11.2):
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
		return fmt.Errorf("GET %s: status %d", pr.FinalURL, resp.StatusCode)
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
	pr := t.probe.Load()
	if pr == nil || pr.TotalSize <= 0 {
		return 0
	}
	done := t.bytesDone.Load()
	if done >= pr.TotalSize {
		return 0
	}
	sp := t.speed.Load()
	if sp <= 0 {
		return 0
	}
	eta := time.Duration((pr.TotalSize - done) * int64(time.Second) / sp)
	if eta < 0 {
		return 0
	}
	return eta
}

// totalOrDone is the total size, or bytes done if total unknown.
func (t *Task) totalOrDone() int64 {
	if pr := t.probe.Load(); pr != nil && pr.TotalSize > 0 {
		return pr.TotalSize
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

// effectiveChunkSize is the chunk size the current layout was built with:
// the aria2c segment size for that profile, else opts.ChunkSize. Persisted
// in the control file so a resume can rebuild the identical layout.
func (t *Task) effectiveChunkSize() int64 {
	if t.opts.Profile == "aria2c" && t.ariaSplit > 0 {
		pr := t.probe.Load()
		if pr != nil && pr.TotalSize > 0 {
			_, seg := AriaSplit(pr.TotalSize, int64(t.opts.Split), t.opts.MinSplitSize)
			return seg
		}
	}
	return t.opts.ChunkSize
}

// region2ChunkSize is the segment size of the both profile's second engine
// (0 for other profiles / legacy control files).
func (t *Task) region2ChunkSize() int64 {
	if t.engines != nil {
		if pr := t.probe.Load(); pr != nil && pr.TotalSize > 0 {
			_, seg := AriaSplit(pr.TotalSize-t.splitAt, int64(t.opts.Split), t.opts.MinSplitSize)
			return seg
		}
	}
	return 0
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
	pr := t.probe.Load()
	if pr == nil || t.queue == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.controlCreatedAt.IsZero() {
		t.controlCreatedAt = now
	}
	t.lastPersist = now
	// Completed offsets are persisted as ABSOLUTE file offsets. For the both
	// profile the second engine's queue is 0-based within its region, so its
	// offsets are translated here (base + rel).
	var completed []int64
	if t.engines != nil {
		completed = append(completed, t.engines[0].CompletedOffsets()...)
		for _, off := range t.engines[1].CompletedOffsets() {
			completed = append(completed, off+t.engines[1].base)
		}
	} else {
		completed = t.queue.CompletedOffsets()
	}
	cf := &storage.ControlFile{
		URL:       t.url,
		FinalURL:  pr.FinalURL,
		TotalSize: pr.TotalSize,
		ChunkSize: t.effectiveChunkSize(),
		ETag:      pr.ETag,
		Completed: completed,
		// Per-chunk SHA-256 hashes for resume verification — only for chunks
		// recorded as completed (a hash can exist for a chunk whose bytes were
		// written but that never reached MarkDone; those must not be trusted).
		ChunkHashes: t.snapshotChunkHashes(completed),
		// Extended metadata
		CreatedAt:   t.controlCreatedAt,
		UpdatedAt:   now,
		Connections: int(t.conns.Load()),
		UserAgent:   t.opts.UserAgent,
		ODMVersion:  version.Version,
		Checksum:    t.opts.Checksum,
		// Profile metadata for layout reconstruction on resume.
		Profile:          t.opts.Profile,
		SplitAt:          t.splitAt,
		Region2ChunkSize: t.region2ChunkSize(),
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
	pr := t.probe.Load()
	if pr == nil || pr.SingleStream || pr.TotalSize <= 0 {
		return nil
	}
	spans := t.queue.CompletedSpans(t.opts.ChunkSize, pr.TotalSize, resumeVerifyChunks)
	for _, s := range spans {
		end := s.Start + resumeProbeLen - 1
		if end > s.End {
			end = s.End
		}
		want := int(end - s.Start + 1)
		chkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := t.client.GetRange(chkCtx, pr.FinalURL, s.Start, end)
		if err != nil {
			cancel()
			return fmt.Errorf("resume check at %d: %w", s.Start, err)
		}
		got := make([]byte, want)
		_, rerr := io.ReadFull(resp.Body, got)
		resp.Body.Close()
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

// storeChunkHash records the SHA-256 hex digest of a successfully completed
// chunk, keyed by its Start offset. Guarded by t.mu — the same lock
// persistControl holds when reading the map, so no second mutex is needed.
// This runs once per completed chunk, never on the copy hot loop.
func (t *Task) storeChunkHash(start int64, sum string) {
	t.mu.Lock()
	t.chunkHashes[start] = sum
	t.mu.Unlock()
}

// clearChunkHashes drops all recorded hashes — used when a resume integrity
// check fails and the download restarts from scratch, so stale hashes can't
// leak into the fresh run's checkpoints.
func (t *Task) clearChunkHashes() {
	t.mu.Lock()
	t.chunkHashes = make(map[int64]string)
	t.mu.Unlock()
}

// restoreChunkHashes seeds the in-memory hash map from a control file being
// resumed, so hashes of previously-completed chunks survive into subsequent
// checkpoints instead of being dropped (which would downgrade the next resume
// to the legacy server-compare fallback). Only hashes for offsets the queue
// actually accepted as completed are carried over. Caller holds no lock.
func (t *Task) restoreChunkHashes(cf *storage.ControlFile) {
	if len(cf.ChunkHashes) == 0 || t.queue == nil {
		return
	}
	done := t.queue.CompletedOffsets()
	t.mu.Lock()
	for _, off := range done {
		if sum, ok := cf.ChunkHashes[off]; ok {
			t.chunkHashes[off] = sum
		}
	}
	t.mu.Unlock()
}

// snapshotChunkHashes returns the recorded hashes for the given completed
// offsets — the subset of t.chunkHashes that has actually been marked done.
// Called from persistControl with t.mu held.
func (t *Task) snapshotChunkHashes(completed []int64) map[int64]string {
	if len(t.chunkHashes) == 0 {
		return nil
	}
	out := make(map[int64]string, len(completed))
	for _, off := range completed {
		if sum, ok := t.chunkHashes[off]; ok {
			out[off] = sum
		}
	}
	return out
}

// verifyResumedData checks the integrity of completed chunks before a resume
// trusts them. It composes TWO independent guarantees:
//
//   - Local disk integrity: when every completed chunk has a recorded per-chunk
//     SHA-256 hash (control files written by this version), each completed
//     chunk's on-disk bytes are hashed and compared — catching local
//     corruption/truncation with no network traffic. Partial coverage (e.g. a
//     hash-less legacy control file that was resumed once: new chunks get
//     hashes, legacy completed chunks don't) does NOT fail the resume; it
//     simply downgrades to the legacy server-side compare.
//
//   - Server drift: the sampled server-side compare (verifyResumedChunks)
//     detects that the server replaced the file with same-size content since
//     the original download. The stored hashes are of the ORIGINAL bytes, so
//     they cannot catch such a replacement by themselves — this is why the
//     two checks are complementary, never alternatives. It no-ops for
//     single-stream/sizeless downloads.
//
// Either failure → the caller re-downloads from scratch.
func (t *Task) verifyResumedData(ctx context.Context, cf *storage.ControlFile) error {
	// Local disk integrity, per-chunk hashes (only when coverage is complete).
	if t.hasFullHashCoverage(cf) {
		if err := t.verifyResumedHashes(cf); err != nil {
			return err
		}
	}
	// Server drift, sampled compare — independent of hashes.
	return t.verifyResumedChunks(ctx)
}

// hasFullHashCoverage reports whether every completed chunk in cf has a
// recorded per-chunk hash — the precondition for hash-based local
// verification. Partial coverage must not fail a resume: it just falls back
// to the legacy server-side compare for the whole set.
func (t *Task) hasFullHashCoverage(cf *storage.ControlFile) bool {
	if cf == nil || len(cf.ChunkHashes) == 0 {
		return false
	}
	for off := range cf.CompletedOffsets() {
		if _, ok := cf.ChunkHash(off); !ok {
			return false
		}
	}
	return true
}

// verifyResumedHashes verifies every completed chunk recorded in cf against
// the on-disk bytes, using the per-chunk SHA-256 hashes captured while the
// chunk was first downloaded and persisted in the control file. This catches
// LOCAL corruption of already-written chunks — flipped/zero-filled bytes,
// truncation, a partial write. Server-side drift is covered separately by
// verifyResumedChunks (the stored hashes are of the original bytes, so they
// cannot detect a same-size replacement). A single mismatch fails the whole
// resume so the caller re-downloads from scratch.
func (t *Task) verifyResumedHashes(cf *storage.ControlFile) error {
	pr := t.probe.Load()
	if pr == nil || pr.TotalSize <= 0 {
		return nil
	}
	offsets := cf.CompletedOffsets()
	starts := make([]int64, 0, len(offsets))
	for off := range offsets {
		starts = append(starts, off)
	}
	// Deterministic order: error diagnostics should name a stable chunk, not
	// whichever map iteration happened to visit first.
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	buf := make([]byte, 64*1024)
	for _, off := range starts {
		want, ok := cf.ChunkHash(off)
		if !ok {
			return fmt.Errorf("resume hash check at %d: no recorded hash for completed chunk", off)
		}
		start, end := t.chunkSpan(off)
		if end < start {
			return fmt.Errorf("resume hash check at %d: invalid span [%d, %d]", off, start, end)
		}
		h := sha256.New()
		for pos := start; pos <= end; {
			n := int64(len(buf))
			if pos+n-1 > end {
				n = end - pos + 1
			}
			if _, err := t.disk.ReadAt(buf[:n], pos); err != nil {
				return fmt.Errorf("resume hash check at %d: read: %w", off, err)
			}
			_, _ = h.Write(buf[:n])
			pos += n
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("resume hash check at %d: on-disk data differs from recorded hash", off)
		}
	}
	return nil
}

// chunkSpan returns the byte span [start, end] that a chunk beginning at
// `start` occupies in the current layout, mirroring NewChunkQueue's split
// arithmetic. The single-stream whole-file chunk (Start=0, End=-1) spans the
// entire known size.
func (t *Task) chunkSpan(start int64) (int64, int64) {
	pr := t.probe.Load()
	if pr.SingleStream {
		return 0, pr.TotalSize - 1
	}
	// Mirror NewChunkQueue's silent default: a non-positive ChunkSize would
	// otherwise produce end = start-1 and spuriously fail every hash verify.
	cs := t.opts.ChunkSize
	if cs < 1 {
		cs = defaultChunkSize
	}
	end := start + cs - 1
	if pr.TotalSize > 0 && end >= pr.TotalSize {
		end = pr.TotalSize - 1
	}
	return start, end
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
//
// The pause mechanism is a broadcast-close wake-up, NOT a one-shot signal:
//   - Pause sets the `paused` flag; workers rendezvous in pauseGate (below),
//     which blocks on the current pauseC channel.
//   - Unpause clears the flag, CLOSES pauseC, and installs a fresh channel.
//     Close is a broadcast: every worker blocked in pauseGate wakes, re-checks
//     the flag under t.mu, and proceeds. A single send on a buffered(1) channel
//     would instead wake only ONE of N blocked workers and leave the rest
//     blocked forever (Start's workerWg.Wait would never return) — that is the
//     bug this design replaces. The fresh channel ensures the next Pause has
//     an open channel for workers to block on again (a closed channel would
//     let them spin instead of sleeping).
//
// The gate reads the channel atomically with the flag (under t.mu), so a
// worker can never block on a channel that a concurrent Unpause has already
// closed-and-replaced. See pauseGate.
func (t *Task) Pause() {
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	t.setState(StatePaused)
}

func (t *Task) Unpause() {
	t.mu.Lock()
	if t.paused {
		t.paused = false
		close(t.pauseC)                // broadcast: wake ALL workers in pauseGate
		t.pauseC = make(chan struct{}) // fresh channel for the next pause cycle
	}
	t.mu.Unlock()
	t.setState(StateActive)
}

// pauseGate blocks the calling worker while the task is paused, returning when
// the task is unpaused (or ctx is cancelled). It is the worker-loop pause
// gate: a worker that reaches it while paused sleeps on pauseC instead of
// burning CPU or draining the queue.
//
// Correctness: the channel is read under t.mu — atomically with the `paused`
// flag — so the wait target is always the channel current for this pause
// cycle. Unpause closes that channel (waking every waiter) before replacing
// it, so no worker can be left sleeping on a stale channel after unpausing.
// The `for t.paused` re-check handles a pause that races the wake-up: a worker
// woken by a close re-locks, sees paused==true (a new Pause won the race),
// and blocks on the freshly-installed channel.
func (t *Task) pauseGate(ctx context.Context) {
	t.mu.Lock()
	for t.paused {
		ch := t.pauseC
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
		t.mu.Lock()
	}
	t.mu.Unlock()
}

func (t *Task) Cancel() {
	t.cancelled.Store(true)
	if t.cancel != nil {
		t.cancel()
	}
}

// CompletedOffsets returns the completed chunk offsets; it takes the queue's
// own lock. Implements the workQueue interface shared with StaticQueue.
func (q *ChunkQueue) CompletedOffsets() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, 0, len(q.completed))
	for k := range q.completed {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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
