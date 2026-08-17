package download

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// Manager glues transport + tasks + scheduler together for the CLI path and is
// reused (in reference form) by the RPC daemon. It builds tasks from URLs,
// tracks them by id, owns the shared transport client + global rate limiter
// (so one pool spans the whole batch), and exposes checksum verification.
type Manager struct {
	client   *transport.Client // HTTP/1.1-only (odm profile)
	h2client *transport.Client // HTTP/2-enabled (aria2c/both/smart profiles); nil unless needed
	lim      *ratelimit.Limiter
	opts     ExecOptions
	logf     LogFn // per-engine Logger; nil ⇒ silent hot path (PRD §6.2 --log-level)

	nextID atomic.Int64
	mu     atomic.Pointer[map[TaskID]*Task] // registry for RPC tellActive/tellWaiting
}

// ExecOptions is the subset of *config.Options the Manager exercises during a
// download. Mirrors TaskOptions + a couple of batch-level knobs.
type ExecOptions struct {
	Dir           string
	OutFile       string // single-file override (-o)
	Connections   int
	MaxConn       int
	SplitFile     int
	Retry         int
	RetryWait     time.Duration
	Continue      bool
	ChunkSize     int64
	Timeout       time.Duration
	Checksum      string // "algo:hex" or ""
	LimitRate     string // "5M"/"500K"/""=unlimited
	TaskLimitRate string // "2M"/""=unlimited — per-task cap
	UserAgent     string
	Headers       []string
	Referer       string
	Proxy         string
	CheckCert     bool
	MaxRedirect   int

	Profile          string // engine profile: odm|aria2c|both|smart ("" = odm)
	Split            int    // aria2c: --split (number of segments), default 5
	MinSplitSize     int64  // aria2c: --min-split-size (default 20 MiB)
	MaxConnPerServer int    // aria2c: -x (per-server connection cap, default 1)
}

// NewManager builds a Manager. The underlying transport.Client + rate limiter
// are constructed here so a single client limiter pool spans the whole batch.
// logf, when non-nil, is injected into every Task so engine progress (probe
// start, resume hits, chunk retries) honours --log/--log-level; nil keeps the
// hot path silent and is what the existing tests assume.
func NewManager(opts ExecOptions, logf LogFn) (*Manager, error) {
	cli, err := transport.NewClient(transport.ClientConfig{
		UserAgent:        opts.UserAgent,
		Headers:          opts.Headers,
		Referer:          opts.Referer,
		Proxy:            opts.Proxy,
		CheckCertificate: opts.CheckCert,
		Timeout:          opts.Timeout,
		MaxRedirect:      opts.MaxRedirect,
	})
	if err != nil {
		return nil, err
	}
	lim, err := ratelimit.New(opts.LimitRate)
	if err != nil {
		return nil, err
	}
	m := &Manager{client: cli, lim: lim, opts: opts, logf: logf}
	// The h2-enabled client is only needed by profiles that speak HTTP/2
	// (aria2c, both region2, smart may pick h2). Eager construction is fine —
	// NewClient does no I/O — and keeps ClientFor lock-free.
	if opts.Profile != "" && opts.Profile != "odm" {
		h2, err := transport.NewClient(transport.ClientConfig{
			UserAgent:        opts.UserAgent,
			Headers:          opts.Headers,
			Referer:          opts.Referer,
			Proxy:            opts.Proxy,
			CheckCertificate: opts.CheckCert,
			Timeout:          opts.Timeout,
			MaxRedirect:      opts.MaxRedirect,
			HTTP2:            true,
		})
		if err != nil {
			return nil, err
		}
		m.h2client = h2
	}
	empty := map[TaskID]*Task{}
	m.mu.Store(&empty)
	return m, nil
}

// Client exposes the transport client (RPC tests reuse it).
func (m *Manager) Client() *transport.Client { return m.client }

// ClientFor returns the transport client for the given profile: the h1-only
// client for odm, the h2-enabled one for aria2c/both/smart (falling back to
// h1 when the profile doesn't need h2 or the h2 client was never built).
func (m *Manager) ClientFor(profile string) *transport.Client {
	if profile == "aria2c" || profile == "both" || profile == "smart" {
		if m.h2client != nil {
			return m.h2client
		}
	}
	return m.client
}

// Limiter exposes the global limiter.
func (m *Manager) Limiter() *ratelimit.Limiter { return m.lim }

// NewTask is the TaskMaker the Scheduler calls. It produces a Task bound to the
// Manager's client + limiter and records it in the task map so the RPC layer
// can enumerate tellActive/tellWaiting/etc. The returned conns is a default;
// the Scheduler overrides it with the Balancer's per-file allocation value
// passed through the plan.
func (m *Manager) NewTask(url string, _ int) (*Task, int, error) {
	id := TaskID(fmt.Sprintf("odm-%03d", m.nextID.Add(1)))
	conns := m.opts.Connections
	profile := m.opts.Profile
	if profile == "" {
		profile = "odm"
	}
	t := NewTask(id, url, TaskOptions{
		OutputName:       m.opts.OutFile,
		Dir:              m.opts.Dir,
		Retry:            m.opts.Retry,
		RetryWait:        m.opts.RetryWait,
		Continue:         m.opts.Continue,
		ChunkSize:        m.opts.ChunkSize,
		Timeout:          m.opts.Timeout,
		UserAgent:        m.opts.UserAgent,
		Checksum:         m.opts.Checksum,
		TaskLimitRate:    m.opts.TaskLimitRate,
		Profile:          profile,
		Split:            m.opts.Split,
		MinSplitSize:     m.opts.MinSplitSize,
		MaxConnPerServer: m.opts.MaxConnPerServer,
	}, m.ClientFor(profile), m.lim, m.logf)
	// Region 2 of the both profile speaks HTTP/2 — attach the Manager's h2
	// client so Start can give it to the second engine.
	if m.h2client != nil {
		t.SetH2Client(m.h2client)
	}
	m.track(id, t)
	return t, conns, nil
}

// maxTrackedTasks bounds the Manager's task registry. The registry backs RPC
// id lookups (tellStatus/pause/remove/changeOption) and would otherwise grow
// without limit over a long-lived daemon's lifetime. Once the cap is exceeded,
// terminal tasks (completed/error — no longer actionable) are pruned oldest
// first; live/queued tasks are never dropped.
var maxTrackedTasks = 1000

// track inserts a task into the manager's registry (used by RPC tellActive),
// pruning the oldest terminal entries once the cap is exceeded.
//
// The copy-on-write CAS map swap is acceptable here because track() runs only
// on task creation (RPC addUri / batch builds), never on hot download paths —
// the O(n) map copy per call is cheap relative to the work a new task triggers.
// Reads (Task/Tasks via m.mu.Load) stay lock-free, which is what matters for
// the per-tick RPC status polling.
func (m *Manager) track(id TaskID, t *Task) {
	for {
		cur := m.mu.Load()
		nv := make(map[TaskID]*Task, len(*cur)+1)
		maps.Copy(nv, *cur)
		nv[id] = t
		if len(nv) > maxTrackedTasks {
			dropTerminalTasks(nv, len(nv)-maxTrackedTasks)
		}
		if m.mu.CompareAndSwap(cur, &nv) {
			return
		}
	}
}

// dropTerminalTasks removes up to n registry entries whose state is terminal,
// lowest id first. Terminal tasks can no longer be paused/removed, so dropping
// them only affects tellStatus for very old tasks — the price of a bounded
// registry. If fewer than n are terminal (e.g. most are still running) the map
// stays slightly over the cap rather than dropping live work.
func dropTerminalTasks(m map[TaskID]*Task, n int) {
	if n <= 0 {
		return
	}
	ids := make([]TaskID, 0, len(m))
	for id, t := range m {
		if s := t.State(); s == StateCompleted || s == StateError {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := 0; i < n && i < len(ids); i++ {
		delete(m, ids[i])
	}
}

// Tasks returns a snapshot of all tasks the manager has built (RPC enumeration).
func (m *Manager) Tasks() map[TaskID]*Task {
	cur := *m.mu.Load()
	out := make(map[TaskID]*Task, len(cur))
	maps.Copy(out, cur)
	return out
}

// Task returns the task with the given id, or nil if unknown. Used by the RPC
// daemon's pause/unpause/remove/tellStatus handlers.
func (m *Manager) Task(id TaskID) *Task {
	cur := *m.mu.Load()
	return cur[id]
}

// verifyChecksum is the shared checksum core: hash the file at path and compare
// against expectHex. Used by Task (which verifies the actual written file —
// including a Content-Disposition-derived name — so a server-side rename can't
// make the CLI check the wrong path).
func verifyChecksum(path, algo, expectHex string) error {
	var h hash.Hash
	switch strings.ToLower(algo) {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	default:
		return fmt.Errorf("unsupported checksum algo %q", algo)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("checksum read: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(expectHex)) {
		return fmt.Errorf("checksum mismatch: got %s want %s", got, expectHex)
	}
	return nil
}

// Exit codes from §13.
const (
	ExitOK        = 0
	ExitGeneral   = 1
	ExitNetwork   = 2
	ExitPartial   = 3
	ExitCancelled = 4
)

// ExitCodeFrom counts succeeded/failed/cancelled to produce the right §13 code.
func ExitCodeFrom(succeeded, failed, cancelled int) int {
	switch {
	case cancelled > 0:
		return ExitCancelled
	case failed == 0 && succeeded == 0:
		return ExitGeneral
	case failed == 0:
		return ExitOK
	case succeeded == 0:
		return ExitNetwork // all failed
	default:
		return ExitPartial // some succeeded, some failed
	}
}

// ResolveDest resolves the path a task for `url` would write to. Used by the
// confirmation prompt before the download actually starts (§9).
func (m *Manager) ResolveDest(url string) string {
	name := m.opts.OutFile
	if name == "" {
		name = filepath.Base(url)
		if name == "" || name == "." || name == "/" {
			name = "download.bin"
		}
	}
	return filepath.Join(m.opts.Dir, name)
}

// Run runs a single task directly (Mode A path) on the given URL+conns and
// returns its error. Used by the tests for single-file scenarios.
func (m *Manager) Run(ctx context.Context, url string, conns int) error {
	t, _, err := m.NewTask(url, 0)
	if err != nil {
		return err
	}
	return t.Start(ctx, conns, nil)
}
