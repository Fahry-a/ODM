package download

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path/filepath"
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
	client *transport.Client
	lim    *ratelimit.Limiter
	opts   ExecOptions
	logf   LogFn // per-engine Logger; nil ⇒ silent hot path (PRD §6.2 --log-level)

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
	MaxRedirect   int
	Checksum      string // "algo:hex" or ""
	LimitRate     string // "5M"/"500K"/""=unlimited
	TaskLimitRate string // "2M"/""=unlimited — per-task cap
	UserAgent     string
	Headers       []string
	Referer       string
	Proxy         string
	CheckCert     bool
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
		MaxRedirect:      opts.MaxRedirect,
		Timeout:          opts.Timeout,
	})
	if err != nil {
		return nil, err
	}
	lim, err := ratelimit.New(opts.LimitRate)
	if err != nil {
		return nil, err
	}
	m := &Manager{client: cli, lim: lim, opts: opts, logf: logf}
	empty := map[TaskID]*Task{}
	m.mu.Store(&empty)
	return m, nil
}

// Client exposes the transport client (RPC tests reuse it).
func (m *Manager) Client() *transport.Client { return m.client }

// Limiter exposes the global limiter.
func (m *Manager) Limiter() *ratelimit.Limiter { return m.lim }

// Opts exposes the manager's options (tests + RPC inspect them).
func (m *Manager) Opts() ExecOptions { return m.opts }

// NewTask is the TaskMaker the Scheduler calls. It produces a Task bound to the
// Manager's client + limiter and records it in the task map so the RPC layer
// can enumerate tellActive/tellWaiting/etc. The returned conns is a default;
// the Scheduler overrides it with the Balancer's per-file allocation value
// passed through the plan.
func (m *Manager) NewTask(url string, _ int) (*Task, int, error) {
	id := TaskID(fmt.Sprintf("odm-%03d", m.nextID.Add(1)))
	conns := m.opts.Connections
	t := NewTask(id, url, TaskOptions{
		OutputName:    m.opts.OutFile,
		Dir:           m.opts.Dir,
		Retry:         m.opts.Retry,
		RetryWait:     m.opts.RetryWait,
		Continue:      m.opts.Continue,
		ChunkSize:     m.opts.ChunkSize,
		Timeout:       m.opts.Timeout,
		MaxRedirect:   m.opts.MaxRedirect,
		UserAgent:     m.opts.UserAgent,
		Checksum:      m.opts.Checksum,
		TaskLimitRate: m.opts.TaskLimitRate,
	}, m.client, m.lim, m.logf)
	m.track(id, t)
	return t, conns, nil
}

// track inserts a task into the manager's registry (used by RPC tellActive).
func (m *Manager) track(id TaskID, t *Task) {
	for {
		cur := m.mu.Load()
		nv := make(map[TaskID]*Task, len(*cur)+1)
		maps.Copy(nv, *cur)
		nv[id] = t
		if m.mu.CompareAndSwap(cur, &nv) {
			return
		}
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

// VerifyChecksum streams the downloaded file through the requested hash and
// compares against the expected hex digest. Returns nil on match.
func (m *Manager) VerifyChecksum(path, algo, expectHex string) error {
	return verifyChecksum(path, algo, expectHex)
}

// verifyChecksum is the shared checksum core: hash the file at path and compare
// against expectHex. Used by Manager.VerifyChecksum and by Task (which verifies
// the actual written file — including a Content-Disposition-derived name — so a
// server-side rename can't make the CLI check the wrong path).
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

// Version is the ODM release string; surfaced over RPC (odm.getVersion) and used
// in the default User-Agent. Defined here (not import cycling from config) so
// the engine + RPC layer can read it without depending on config at runtime.
const Version = "odm/1.1.0"

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

// ResolveDest returns the path a task for `url` would write to. Used by the
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

// ErrNoTasks is returned by bootstrap helpers when there's nothing to schedule.
var ErrNoTasks = errors.New("no tasks to schedule")

// Run is a convenience wrapper used by tests: it runs a single task directly
// (Mode A path) on the given URL+conns and returns its error. The CLI flow
// uses RunBatch for the full Balancer-driven scheduling; Run keeps the simple
// single-file case one call away.
func (m *Manager) Run(ctx context.Context, url string, conns int) error {
	t, _, err := m.NewTask(url, 0)
	if err != nil {
		return err
	}
	return t.Start(ctx, conns, nil)
}
