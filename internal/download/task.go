package download

import (
	"context"
	"fmt"
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
