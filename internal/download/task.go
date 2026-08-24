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
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
// UI color states (queued/dimming, downloading=yellow, retry/error=red,
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

	// stallDecayWindow is how long a zero-byte window must last before the
	// displayed speed decays to 0 (see rateMeasure.tick). Shorter than this a
	// quiet window reads as jitter, not a stall.
	stallDecayWindow = 2 * time.Second

	// throttleCooldown is how long after the latest 429 before a successful
	// chunk restores the user's configured rate (see downloadChunk/throttleOK).
	throttleCooldown = 30 * time.Second
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

// Task represents one file's download. A Task owns:
//   - its probe result (size + range-support)
//   - a chunk queue
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

	// resumeETag is the control file's ETag when a --continue resume passed
	// validation. Sent as If-Range on every ranged GET: if the resource changed
	// since the interrupted run, the server answers 200 (not 206) and the chunk
	// path treats that as the drift signal it already handles (transient retry
	// → permanent failure of THIS layout) instead of stitching old chunks to
	// new bytes. "" = no If-Range (fresh downloads).
	resumeETag string
	probe      atomic.Pointer[transport.ProbeResult]
	disk       *storage.File
	queue      workQueue
	outPath    string

	// ariaSplit is the effective segment count computed for the aria2c
	// profile (0 for odm). Used to cap workers at the segment count and to
	// rebuild the layout on resume.
	ariaSplit int64

	// both profile: engines[0] covers [0, splitAt) on the h1 client,
	// engines[1] covers [splitAt, end) on the h2 client. nil for other
	// profiles. regionConns holds the worker count per engine.
	//
	// layoutMu guards engines/splitAt/regionConns/single against the RPC
	// daemon: the task is admitted the moment NewTask returns, so a
	// changeOption → AdjustConns can read currentEngine while Start is still
	// building the layout. All writes happen in Start (single goroutine);
	// currentEngine reads under the same lock.
	layoutMu    sync.Mutex
	engines     []*Engine
	regionConns []int
	splitAt     int64

	// single is the hoisted engine for single-profile tasks (odm/aria2c),
	// cached on first currentEngine() call so workers don't allocate a fresh
	// struct per spawn.
	single *Engine

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

	// mirrorIdx rotates chunk requests across opts.Mirrors (round-robin).
	mirrorIdx atomic.Uint64

	// lastThrottle tracks the most recent 429 so throttleOK only restores the
	// configured rate after a quiet period, not on the first healthy chunk.
	lastThrottle atomic.Int64 // unix nanos; 0 = never throttled

	// persistWarned gates the one-shot log warning when SaveControl fails
	// (checkpoints fire per-chunk-count/time — one warn per task, not per try).
	persistWarned atomic.Bool

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

	Collision string // what to do when the destination exists: ""|"overwrite" | "rename" (--auto-rename) | "skip" (--skip-existing)

	ChecksumURL string   // --checksum-url: fetch "algo:digest" from this sidecar URL before downloading
	Mirrors     []string // --mirror (repeatable): alternate URLs serving the SAME file; chunks rotate across them
}

// uniqueName returns dir/name rewritten to base.N.ext with the lowest N≥1
// that doesn't exist yet ("f.tar.gz" → "f.1.tar.gz"): the counter goes before
// filepath.Ext's last extension, so compound extensions stay readable.
func uniqueName(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); err != nil {
			return candidate
		}
	}
}

// sizeOrUnknown renders size for log lines: "?" when unknown (<0).
func sizeOrUnknown(size int64) string {
	if size < 0 {
		return "?"
	}
	return strconv.FormatInt(size, 10)
}

// rateMeasure keeps a short rolling window of bytes vs time to produce a stable
// instantaneous speed for the UI without flooding.
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
// throttle so the UI sink fires on the same cadence ("throttled
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
		} else if elapsed >= stallDecayWindow {
			// No bytes at all this window: decay the displayed speed toward zero
			// so a stalled connection doesn't freeze the bar at its last speed
			// forever.
			rm.bps = 0
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
	// Clamp defensively: Retry < 0 makes downloadChunk's `range Retry+1` run
	// zero iterations (chunks "succeed" without downloading); a negative wait
	// shifts the backoff negative too. Config.Validate rejects these upstream —
	// this guards direct engine users.
	if opts.Retry < 0 {
		opts.Retry = 0
	}
	if opts.RetryWait < 0 {
		opts.RetryWait = 0
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

// SetProfile pins a concrete engine profile on the task so Start skips the
// smart re-resolution (and its extra h2 HEAD probe). The CLI path resolves
// smart per-file after probing and injects the decision here; the RPC daemon
// path leaves it empty and Start resolves normally. Must be called before
// Start, like SetProbe.
func (t *Task) SetProfile(p string) {
	if p != "" {
		t.opts.Profile = p
	}
}

// AdjustConns changes the desired connection count at runtime. When target is
// lower than the current count, excess workers gracefully drain after finishing
// their current chunk (no mid-chunk cancels). When target is higher, additional
// worker goroutines are spawned. Safe to call concurrently with Start.
// Returns true if the adjustment was applied, false if the task has already
// finished (no workers can be spawned).
func (t *Task) AdjustConns(target int, ctx context.Context, sink func(ProgressView)) bool {
	// Clamp to ≥1 defensively (the RPC boundary already rejects <1): target 0
	// would drain every worker while chunks remain, and Start reports the
	// half-empty file as completed. One live worker is always safe.
	if target < 1 {
		target = 1
	}
	if ctx == nil {
		ctx = t.baseCtx
	}
	if sink == nil {
		sink = t.sink
	}
	// A task admitted by the RPC daemon but not yet Started has no baseCtx —
	// the increase path below would spawn workers on a nil context (panic).
	// Start reads connTarget when it launches, so an early adjustment still
	// takes effect; we just can't spawn goroutines for it yet.
	if ctx == nil {
		return false
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
	// --checksum-url: pull the digest from the sidecar before any byte of the
	// payload moves. The sidecar is a small text file ("algo:hash" or a bare
	// hash, as emitted by sha256sum et al); parsing accepts both. A fetch or
	// parse failure fails the task — silently skipping verification would
	// defeat the point of asking for it.
	if t.opts.ChecksumURL != "" && t.opts.Checksum == "" {
		spec, err := t.fetchChecksumSpec(ctx)
		if err != nil {
			t.setState(StateError)
			t.emitFinal(progressSink)
			return fmt.Errorf("checksum-url: %w", err)
		}
		t.opts.Checksum = spec
		t.logf("info", "checksum from %s: %s", t.opts.ChecksumURL, spec)
	}
	// Smart profile: decide the concrete engine now that the probe answered
	// range support + size, and check h2 readiness through the h2 client.
	// The CLI path already resolved smart to a concrete profile (SetProfile,
	// after the probe pass) and injected it, so this only runs for tasks whose
	// profile is still literally "smart" — the RPC daemon path where Start
	// probes lazily.
	if t.opts.Profile == "smart" {
		// The scheduler applies the Balancer's per-file allocation via
		// SetConns BEFORE Start runs, so t.conns holds this file's real budget
		// (1 in batch mode) — using the raw Start `conns` (the global default
		// from the TaskMaker) would make smart choose "both" for a batch file
		// that actually gets 1 connection. Fall back to the parameter when
		// nothing was set (Manager.Run's direct path).
		perFile := int(t.conns.Load())
		if perFile < 1 {
			perFile = conns
		}
		profile, reason := ChooseProfile(ServerCapabilities{
			TotalSize:     pr.TotalSize,
			SupportsRange: pr.SupportsRange,
			SingleStream:  pr.SingleStream,
			HTTP2Ready:    t.h2Client != nil && t.h2Client.SupportsHTTP2(ctx, t.url),
			Conns:         perFile,
		})
		t.logf("info", "smart profile: chose %q (%s)", profile, reason)
		t.opts.Profile = profile
	}
	if pr.Filename == "" || (t.opts.OutputName != "") {
		// Filename refinement publishes a COPIED probe: pr is shared with
		// Snapshot() readers, and in-place mutation raced with them.
		pr2 := *pr
		if pr2.Filename == "" {
			pr2.Filename = deriveFilename(pr2.FinalURL, t.opts.OutputName)
		}
		if t.opts.OutputName != "" {
			pr2.Filename = t.opts.OutputName // explicit -o wins
		}
		t.probe.Store(&pr2)
		pr = &pr2
	}
	t.setState(StateActive)

	// 2. Resolve paths + attempt resume.
	dir := t.opts.Dir
	outName := flattenFilename(pr.Filename)
	if outName == "" {
		outName = "download.bin"
	}
	t.outPath = filepath.Join(dir, outName)

	// Collision policy — applies only when this run is NOT resuming: a
	// resumable .odm control file owns this destination and must never be
	// renamed away from it. (--continue is on by default, so gate on the
	// control file's presence, not on the flag.)
	resumable := false
	if t.opts.Continue {
		if _, cerr := storage.LoadControl(t.outPath); cerr == nil {
			resumable = true
		}
	}
	if !resumable {
		switch t.opts.Collision {
		case "skip":
			if st, err := os.Stat(t.outPath); err == nil && st.Mode().IsRegular() {
				// Size match when known → genuinely complete, skip as success;
				// otherwise the file exists but we can't vouch for it.
				if pr.TotalSize > 0 && st.Size() == pr.TotalSize {
					t.logf("info", "skipping %s: already downloaded (%d bytes)", outName, st.Size())
					t.bytesDone.Store(st.Size())
					t.setState(StateCompleted)
					// Emit so the skipped task appears in the final UI frame /
					// RPC state instead of vanishing (every other completion
					// path emits before returning).
					if progressSink != nil {
						progressSink(t.Snapshot())
					}
					return nil
				}
				t.logf("warn", "--skip-existing: %s exists with a different size (%d ≠ %s), re-downloading", outName, st.Size(), sizeOrUnknown(pr.TotalSize))
			}
		case "rename":
			if _, err := os.Stat(t.outPath); err == nil {
				outName = uniqueName(dir, outName)
				// Publish via a COPIED probe: pr is shared with Snapshot()
				// readers (UI/RPC pollers), and mutating pr.Filename in place
				// raced with them. Same for deriveFilename below.
				pr2 := *pr
				pr2.Filename = outName
				t.probe.Store(&pr2)
				pr = &pr2
				t.outPath = filepath.Join(dir, outName)
				t.logf("info", "%s exists — saving as %s", filepath.Base(t.outPath), outName)
			}
		}
	}

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
	t.layoutMu.Lock()
	t.engines = nil
	t.single = nil
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
		if conns < 2 {
			// One connection can't split across two regions without doubling
			// the TCP budget (max(1,1)+max(1,0) would spawn 2 workers on a
			// 1-connection budget). Degrade to the single-region odm engine.
			isBoth = false
			t.logf("info", "both profile: %d connection(s), using single-region odm engine", conns)
		}
	}
	if isBoth && qs > 0 {
		conns1 := max(1, conns/2)
		conns2 := max(1, conns-conns1)
		t.regionConns = []int{conns1, conns2}
		t.splitAt = qs / 2
		if t.splitAt < 1 {
			t.splitAt = 1
		}
		mid := t.splitAt
		n2, _ := AriaSplit(qs-mid, int64(t.opts.Split), t.opts.MinSplitSize)
		t.engines = []*Engine{
			// region1 = odm work-stealing → h1 (t.client is ALWAYS the h1
			// client now); region2 = static split → h2 when available.
			{q: NewChunkQueue(mid, t.opts.ChunkSize), client: t.client, base: 0},
			{q: NewStaticQueue(qs-mid, n2), client: t.engineClient(true), base: mid},
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
	t.queue = q         // set early: resume restore/verification below reads the queue
	t.layoutMu.Unlock() // layout settled; readers (currentEngine) may proceed

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
					// Pin the control file's ETag for If-Range on every ranged
					// GET this run (empty when the server never sent one).
					t.resumeETag = cf.ETag
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
				// Rebuild both engines with the same layout math (region1 h1,
				// region2 h2-when-available — same routing as fresh Start).
				mid := t.splitAt
				n2, _ := AriaSplit(qs-mid, int64(t.opts.Split), t.opts.MinSplitSize)
				t.engines = []*Engine{
					{q: NewChunkQueue(mid, t.opts.ChunkSize), client: t.client, base: 0},
					{q: NewStaticQueue(qs-mid, n2), client: t.engineClient(true), base: mid},
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
	// Don't clobber a connTarget an RPC changeOption already raised before
	// Start ran: keep the larger of (param, current target).
	if cur := t.connTarget.Load(); int(cur) > conns {
		conns = int(cur)
	}
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
		// The displayed/live connection count must reflect the CAP, not the raw
		// budget: with 4 segments and -c 16 the UI used to show [x16] while only
		// 4 workers existed.
		conns = workerCount
	}
	t.conns.Store(int32(conns))
	t.connTarget.Store(int32(conns))
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

	// A cancelled context (^C / SIGTERM) is not an error: partial bytes are
	// preserved and --continue resumes. Paint the task as paused instead of
	// the red error glyph so the final screen matches the "cancelled" summary.
	if err := ctx.Err(); err != nil {
		t.setState(StatePaused)
		t.persistControl()
		t.emitFinal(progressSink)
		return err
	}
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
// Reads under layoutMu so a daemon-side AdjustConns never races Start's layout
// writes; the single-profile engine is hoisted after first build so the worker
// spawn loop doesn't allocate a fresh struct per connection.
func (t *Task) currentEngine() *Engine {
	t.layoutMu.Lock()
	defer t.layoutMu.Unlock()
	if t.engines != nil {
		return t.engines[0]
	}
	if t.single == nil {
		t.single = &Engine{q: t.queue, client: t.engineClient(isStaticQueue(t.queue)), base: 0}
	}
	return t.single
}

// engineClient picks the transport client by ENGINE kind, not profile string:
// a static split (aria2c model) multiplexes its segments over h2 when an h2
// client exists; fixed-chunk work-stealing (odm model — including a both task
// degraded to one region, whose opts.Profile still reads "both") needs the
// h1-only client so its N workers stay N separate TCP connections. static=true
// for every NewStaticQueue-backed engine.
func (t *Task) engineClient(static bool) *transport.Client {
	if static && t.h2Client != nil {
		return t.h2Client
	}
	return t.client
}

// isStaticQueue reports whether q is a StaticQueue (aria2c-model static split)
// — the engine-kind signal engineClient routes on.
func isStaticQueue(q workQueue) bool {
	_, ok := q.(*StaticQueue)
	return ok
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
func statusErr(msg string, status int) error {
	return transport.PermanentWrap(fmt.Errorf("%s: status %d", msg, status), status)
}

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
	// Divide FIRST: (remaining * 1e9) overflows int64 for remaining > ~9.2 GiB
	// (a 20 GiB remainder used to wrap negative → ETA showed 0).
	eta := time.Duration((pr.TotalSize-done)/sp) * time.Second
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

// fetchChecksumSpec downloads the sidecar at opts.ChecksumURL and parses it
// into an "algo:hex" spec. Accepted forms: "algo:<hash>", "<64-hex>" (sha256),
// "<40-hex>" (sha1), "<32-hex>" (md5), optionally followed by whitespace and
// the filename (sha256sum output format).
func (t *Task) fetchChecksumSpec(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := t.client.NewGetRequest(ctx, t.opts.ChecksumURL)
	if err != nil {
		return "", err
	}
	resp, err := t.client.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer transport.SkipBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: status %d", t.opts.ChecksumURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return parseChecksumSidecar(string(body))
}

func parseChecksumSidecar(s string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	tok := fields[0]
	algo, hexStr, ok := strings.Cut(tok, ":")
	if !ok {
		switch len(tok) {
		case 64:
			algo, hexStr = "sha256", tok
		case 40:
			algo, hexStr = "sha1", tok
		case 32:
			algo, hexStr = "md5", tok
		default:
			return "", fmt.Errorf("unrecognised digest form %q", tok)
		}
	}
	algo = strings.ToLower(algo)
	switch algo {
	case "md5", "sha1", "sha256":
	default:
		return "", fmt.Errorf("unsupported algorithm %q", algo)
	}
	return algo + ":" + strings.ToLower(hexStr), nil
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
	// On success the control file is removed.
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
	if err := storage.SaveControl(t.outPath, cf); err != nil && !t.persistWarned.Swap(true) {
		// Best-effort by contract (a log failure must never fail the download),
		// but a full disk or permission error silently destroys resume state —
		// warn once per task so the user isn't surprised when --continue
		// doesn't pick up where it left off.
		t.logf("warn", "could not persist resume state %s: %v", storage.ControlPath(t.outPath), err)
	}
}

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

// Pause / Unpause are RPC-facing hooks.
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

// flattenFilename clamps a server-controlled filename (Content-Disposition or
// URL basename) to a single path component: no separators, no dot segments, no
// escape from Dir via filepath.Join. An explicit -o override is NOT passed
// through here — the user chose that name themselves.
//
//   - separators '/' and '\\' are replaced so "a/b" and "a\\b" stay inside Dir
//     (Windows-style separators matter when the same name lands on a Windows
//     share/FS later);
//   - "." and ".." collapse to nothing, so Join(dir, "..") can never escape;
//   - empty results fall back to download.bin.
func flattenFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == os.PathSeparator || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	// A trailing "..foo" is a valid filename; only exact dot segments are
	// dangerous. After separator flattening no path element boundary remains,
	// so Base() is belt-and-braces for exotic FS edge cases.
	if base := filepath.Base(name); base != name && base != "." && base != ".." {
		name = base
	}
	return name
}

// deriveFilename picks an output name from the URL path, or falls back to the
// --output override / "download.bin". Both sources are server-controlled
// (Content-Disposition feeds pr.Filename; the URL path feeds the basename), so
// the result is flattened to a single path component before it can reach
// filepath.Join — see flattenFilename.
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
			return flattenFilename(name)
		}
	}
	return "download.bin"
}
