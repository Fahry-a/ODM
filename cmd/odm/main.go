// Command odm is the Oryn Download Manager binary (PRD §1). It is the single
// entry point that wires the internal packages together: it parses CLI +
// config (internal/config), probes URLs (internal/transport), computes the
// Connection Balancer plan (internal/scheduler), drives the chunk-queue engine
// (internal/download), renders the pacman progress bar (internal/ui), and — in
// --rpc mode — serves the JSON-RPC + WebSocket control surface (internal/rpc).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"odm/internal/config"
	"odm/internal/download"
	"odm/internal/logging"
	"odm/internal/ratelimit"
	"odm/internal/rpc"
	"odm/internal/scheduler"
	"odm/internal/ui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if ee, ok := err.(errExit); ok {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "odm:", ee.msg)
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "odm:", err)
		os.Exit(download.ExitGeneral)
	}
}

// run is the whole program minus os.Exit plumbing, so tests can drive it. It
// never calls os.Exit directly — it returns a sentinel exitError carrying the
// final code, and the caller (main / tests) decides how to surface it.
func run(argv []string) error {
	// pflag's help/version: handle before anything networky. We bind -V/-h onto
	// the same FlagSet that Setup builds, but intercept them ahead of config
	// load so a malformed config never blocks `odm --version`/`--help`.
	if preHelpOrVersion(argv) {
		return nil // printed already
	}

	o, fs, err := config.Setup(argv)
	if err != nil {
		if err == pflag.ErrHelp {
			printUsage(os.Stdout)
			return nil
		}
		printUsage(os.Stderr)
		fmt.Fprintln(os.Stderr)
		return err
	}
	_ = fs

	// Leveled logger (§6.2 --log / --log-level). Default level info; a quiet
	// session with no --log file degrades to a silent engine LogFn so the
	// pacman bar is the only output.
	var logLevel = logging.LevelInfo
	if o.LogLevel != "" {
		lv, lerr := logging.ParseLevel(o.LogLevel)
		if lerr == nil {
			logLevel = lv
		}
	}
	silentEngine := o.Quiet && o.LogFile == ""
	logger, err := logging.New(logLevel, o.LogFile)
	if err != nil {
		return err
	}
	defer logger.Close()
	var engineLog download.LogFn
	if silentEngine {
		engineLog = nil
	} else {
		engineLog = logger.TaskLogFn()
	}

	// --- RPC daemon path (PRD §10): no confirmation prompt (§9). -----------------
	if o.RPC {
		return runRPC(o, engineLog, logger)
	}

	// --- CLI one-shot path --------------------------------------------------------
	if len(o.URLs) == 0 {
		return errExit{code: download.ExitGeneral, msg: "no URLs provided (pass URLs as args, -i <file>, or use --rpc)"}
	}

	exec, err := buildExecOptions(o)
	if err != nil {
		return err
	}
	mgr, err := download.NewManager(exec, engineLog)
	if err != nil {
		return err
	}

	// §5.2 probe every URL up front so the Balancer can do allocation-time
	// reallocation for files that don't support ranges (§5.5). The probe also
	// learns Content-Length for the confirmation prompt (§9) and the progress
	// bar's total.
	ctx, cancel := signalCtx()
	defer cancel()

	files := make([]scheduler.FileInput, 0, len(o.URLs))
	sizes := make(map[string]int64, len(o.URLs))
	probeClient := mgr.Client()
	for _, u := range o.URLs {
		pr, perr := probeClient.Probe(ctx, u)
		if perr != nil {
			logger.Warnf("probe failed for %s: %v", u, perr)
			// Treat an unprobeable URL as range-unsupported and sizeless; the
			// Balancer will give it 1 connection and escalate to single-stream.
			files = append(files, scheduler.FileInput{URL: u, SupportsRange: false})
			sizes[u] = -1
			continue
		}
		files = append(files, scheduler.FileInput{URL: u, SupportsRange: pr.SupportsRange})
		sizes[u] = pr.TotalSize
	}

	plan, err := scheduler.Compute(o.Connections, files, o.SplitFile, o.MaxConnection)
	if err != nil {
		return errExit{code: download.ExitGeneral, msg: err.Error()}
	}
	if plan.Warning != "" {
		logger.Warnf("%s", plan.Warning)
	}

	// §9 confirmation prompt — skipped when -y/--yes OR --quiet (PRD §9).
	if !o.Yes && !o.Quiet {
		ok, err := confirmPlan(o, plan, sizes, mgr)
		if err != nil {
			return err
		}
		if !ok {
			return errExit{code: download.ExitCancelled, msg: "cancelled by user"}
		}
	}

	// Progress renderer: a buffered snapshot channel feeds the RunLoop at the
	// ~100ms cadence the PRD calls out (§11.1 "throttled progress"). The
	// scheduler's ProgressCB pushes into it.
	r := ui.NewRenderer(os.Stdout, o.Quiet)
	snap := make(chan []download.ProgressView, 16)
	qSnap := make(chan []download.ProgressView, 16)
	progCB := func(live, queued []download.ProgressView) {
		// Non-blocking sends so a stalled UI never blocks the engine.
		select {
		case snap <- live:
		default:
		}
		select {
		case qSnap <- queued:
		default:
		}
	}

	sch := scheduler.NewScheduler(plan, mgr.NewTask, progCB)
	uiCtx, uiCancel := context.WithCancel(context.Background())
	defer uiCancel()
	go r.RunLoop(uiCtx, 100*time.Millisecond, snap, qSnap)

	succeeded, failed, runErr := sch.Run(ctx)
	uiCancel()
	// Final frame so the terminal lands on the completed bars.
	r.Frame(nil, nil)

	// Checksum verification (single-file, §16 acceptance). Errors here don't
	// retroactively fail a successful download unless the hash mismatches.
	if o.Checksum != "" && len(o.URLs) == 1 && failed == 0 {
		algo, hexStr, _ := strings.Cut(o.Checksum, ":")
		dest := mgr.ResolveDest(o.URLs[0])
		if cerr := mgr.VerifyChecksum(dest, algo, hexStr); cerr != nil {
			logger.Errorf("checksum: %v", cerr)
			failed++
			succeeded-- // it didn't actually succeed
		}
	}

	code := download.ExitCodeFrom(succeeded, failed, 0)
	printSummary(succeeded, failed, len(o.URLs))
	if runErr != nil && runErr != context.Canceled {
		return errExit{code: code, msg: runErr.Error()}
	}
	return errExit{code: code}
}

// preHelpOrVersion handles the early bail-outs for --help/-h and --version/-V
// before config load, so a broken config never traps the user out of help.
// We only act when one of these is present and not preceded by `--` (which
// pflag treats as "rest are positional").
func preHelpOrVersion(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		switch a {
		case "-h", "--help":
			printUsage(os.Stdout)
			return true
		case "-V", "--version":
			fmt.Println(download.Version)
			return true
		}
	}
	return false
}

// buildExecOptions maps *config.Options → download.ExecOptions, converting the
// second-granular CLI fields into time.Duration and parsing --chunk-size
// ("4M") via the shared ratelimit.ParseRate so byte-suffix rules are uniform.
func buildExecOptions(o *config.Options) (download.ExecOptions, error) {
	chunk, err := parseChunkSize(o.ChunkSize)
	if err != nil {
		return download.ExecOptions{}, err
	}
	dir := o.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return download.ExecOptions{
		Dir:         dir,
		OutFile:     o.OutFile,
		Connections: o.Connections,
		MaxConn:     o.MaxConnection,
		SplitFile:   o.SplitFile,
		Retry:       o.Retry,
		RetryWait:   time.Duration(o.RetryWait) * time.Second,
		Continue:    o.Continue,
		ChunkSize:   chunk,
		Timeout:     time.Duration(o.Timeout) * time.Second,
		MaxRedirect: o.MaxRedirect,
		Checksum:    o.Checksum,
		LimitRate:   o.LimitRate,
		UserAgent:   o.UserAgent,
		Headers:     o.Headers,
		Referer:     o.Referer,
		Proxy:       o.Proxy,
		CheckCert:   o.CheckCertificate,
	}, nil
}

// parseChunkSize turns a "4M"/"512K" string into bytes. Defaults to 4 MiB when
// unset/empty. Reuses ratelimit.ParseRate so the same K/M/G suffix table applies
// to both --chunk-size and --limit-rate.
func parseChunkSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 4 * 1024 * 1024, nil
	}
	n, err := ratelimit.ParseRate(s)
	if err != nil {
		return 0, fmt.Errorf("--chunk-size %q: %w", s, err)
	}
	if n < 1024 {
		return 0, fmt.Errorf("--chunk-size must be at least 1 KiB, got %d", n)
	}
	return n, nil
}

// confirmPlan renders the §9 prompt for the appropriate mode (single-file vs
// batch) and returns the user's Y/n answer.
func confirmPlan(o *config.Options, plan *scheduler.Plan, sizes map[string]int64, mgr *download.Manager) (bool, error) {
	if len(o.URLs) == 1 {
		url := o.URLs[0]
		conns := 1
		if len(plan.Parallel) > 0 {
			conns = plan.Parallel[0].Connections
		}
		size := sizes[url]
		name := mgr.ResolveDest(url)
		// basename for display, full path for destination.
		disp := name
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			disp = name[i+1:]
		}
		return ui.ConfirmSingle(os.Stdin, os.Stdout, disp, name, size, conns)
	}
	rows := ui.RowsFromPlan(plan, sizes)
	connsPerFile := 1
	if o.SplitFile > 0 {
		connsPerFile = o.SplitFile
	}
	return ui.ConfirmBatch(os.Stdin, os.Stdout, rows, connsPerFile, len(plan.Parallel), len(o.URLs))
}

// printSummary writes the final outcome line (§12 step 9).
func printSummary(succeeded, failed, total int) {
	verb := "succeeded"
	if failed > 0 {
		verb = "finished with errors"
	}
	fmt.Fprintf(os.Stdout, "\nodm: %s — %d/%d files %s (%d failed)\n",
		verb, succeeded, total, verb, failed)
}

// signalCtx builds a context cancelled on SIGINT/SIGTERM so ^C aborts cleanly
// and surfaces as exit code 4 (cancelled) rather than a panic.
func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()
	return ctx, cancel
}

// runRPC starts the JSON-RPC + WebSocket server over an empty scheduler daemon
// and serves until odm.shutdown arrives or the process is signalled.
func runRPC(o *config.Options, engineLog download.LogFn, logger *logging.Logger) error {
	exec, err := buildExecOptions(o)
	if err != nil {
		return err
	}
	mgr, err := download.NewManager(exec, engineLog)
	if err != nil {
		return err
	}

	slots := o.Connections
	if slots < 1 {
		slots = config.DefaultConnections
	}

	// Build the WebSocket Broadcaster up front so the scheduler's progress
	// callback (created before the Daemon) and the Server share one fan-out —
	// onDownloadProgress, onDownloadStart and onDownloadComplete all go to the
	// same subscribers (PRD §10.3).
	bc := rpc.NewBroadcaster()
	progTh := newProgressThrottler(bc)
	sch := scheduler.NewEmptyScheduler(slots, mgr.NewTask, progTh.forward)
	daemon := scheduler.NewDaemon(sch, mgr)
	srv := rpc.NewServerWithBroadcaster(daemon, o.RPCSecret, bc)

	// §10.3 lifecycle events: map each task's terminal snapshot onto the right
	// WebSocket event. Registered before Start so the live scheduler's
	// handleComplete observes the hook when it fires.
	daemon.OnComplete(func(v download.ProgressView) {
		srv.OnTaskComplete(v)
	})

	ctx, cancel := signalCtx()
	defer cancel()
	daemon.Start(ctx)
	// When the scheduler winds down (odm.shutdown or signal), close the
	// listener so ListenAndServe returns and the binary exits cleanly.
	shutdown := make(chan struct{})
	daemon.OnDead(func() { close(shutdown) })

	mux := http.NewServeMux()
	srv.Routes(mux)

	host := "127.0.0.1"
	if o.RPCListenAll {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, o.RPCPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Infof("odm RPC listening on %s (secret:%s)", addr, secretWord(o.RPCSecret))
	serveErr := make(chan error, 1)
	go func() { serveErr <- http.Serve(ln, mux) }()

	select {
	case <-shutdown:
		_ = ln.Close()
		<-serveErr
		return nil
	case <-ctx.Done():
		_ = ln.Close()
		<-serveErr
		return errExit{code: download.ExitCancelled, msg: "rpc server interrupted"}
	case err := <-serveErr:
		return err
	}
}

// secretWord reports whether a secret is configured, for the startup banner —
// never echoes the value.
func secretWord(s string) string {
	if s == "" {
		return "off"
	}
	return "on"
}

// progressThrottler coalesces the scheduler's high-frequency progress snapshots
// into coarse WebSocket events so a large batch doesn't flood subscribers. It
// forwards one onDownloadProgress event per live task at most once per interval;
// snapshots between ticks are dropped (the next tick re-reads fresh state from
// the engine, so no progress is lost — only intermediate ones).
type progressThrottler struct {
	bc   *rpc.Broadcaster
	mu   sync.Mutex
	last time.Time
}

// newProgressThrottler wraps bc with a ~250ms emission floor.
func newProgressThrottler(bc *rpc.Broadcaster) *progressThrottler {
	return &progressThrottler{bc: bc}
}

// forward is the scheduler.ProgressCB wired into NewEmptyScheduler. It emits
// onDownloadProgress for every live task at most once per tick; queued tasks
// are not forwarded (they have no live bytes yet) to keep the feed meaningful.
func (p *progressThrottler) forward(live, _ []download.ProgressView) {
	p.mu.Lock()
	if time.Since(p.last) < 250*time.Millisecond {
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	for _, v := range live {
		p.bc.Broadcast(rpc.Event{
			Method: "onDownloadProgress",
			Params: rpc.SnapshotParams(v),
		})
	}
}

// errExit carries a §13 exit code without touching os.Exit, so run() stays
// test-friendly. The sentinel's Error() surfaces a message only when one was
// set; a bare exit-code outcome prints nothing extra (the summary already did).
type errExit struct {
	code int
	msg  string
}

func (e errExit) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return e.msg
}

// printUsage renders the help text. It is deliberately hand-written rather than
// derived from pflag's FlagUsages so the flag table matches the PRD §6.2 list
// exactly (with the documented aliases) — pflag's own formatting drops aliases
// and reorders by category, which is less friendly for a download tool.
func printUsage(w *os.File) {
	fmt.Fprintf(w, `odm %s — Oryn Download Manager (Connection Balancer + pacman bar)

Usage:
  odm [OPTIONS] <URL> [URL ...]        # space-separated URLs (recommended)
  odm [OPTIONS] "URL1,URL2,..."        # legacy comma form (http://-prefixed)
  odm [OPTIONS] -i <file-list.txt>     # URLs from a file, one per line
  odm --rpc [OPTIONS]                  # JSON-RPC + WebSocket daemon

Connection budget (the Balancer auto-splits -c across files):
  -c, --connections  total parallel-connection budget        (default 5)
      --max-connections  ceiling; exceeding it just warns     (default 32)
      --split-file/-sf  connections per file in batch mode   (unset = 1 per file)

Inputs / output:
  -o, --output NAME   output filename (single-file only)
  -d, --dir PATH       destination directory                  (default cwd)
  -i, --input-file FILE  read URL list from FILE

Behavior:
  -y, --yes           skip the confirmation prompt
  -q, --quiet         no progress bar (cron/scripts); also skips the prompt
  -x, --continue      resume an incomplete file via the .odm control file (default on)
      --chunk-size SIZE   work-stealing chunk size, e.g. 4M   (default 4M)

HTTP / network:
      --max-redirect N   redirect hops to follow          (default 5)
      --retry N          retries per segment on failure    (default 3)
      --retry-wait SEC   delay between retries            (default 2)
      --timeout SEC      dial+headers timeout             (default 30)
      --user-agent UA    custom User-Agent               (default %s)
  -H, --header K:V       add a custom header (repeatable)
      --referer URL      set the Referer header
      --proxy URL        http/https/socks5 proxy
      --check-cert BOOL  verify TLS                       (default true)
      --checksum algo:hash  verify md5/sha1/sha256
      --limit-rate RATE   global speed limit, e.g. 5M/500K

Config / logging / RPC:
      --config PATH     config file path               (default /etc/odm/config.conf)
      --log FILE        mirror logs to FILE
      --log-level LEVEL debug|info|warn|error          (default info)
      --rpc             run as the RPC server (daemon)
      --rpc-listen-port PORT                           (default 6900)
      --rpc-listen-all  bind 0.0.0.0 (default 127.0.0.1)
      --rpc-secret TOKEN  auth for RPC ('token:<secret>' param or ?secret=)
  -V, --version         print version and exit
  -h, --help            show this help

Config priority: CLI > ~/.config/odm/config.conf > /etc/odm/config.conf > defaults.
URLs with a literal comma must use space-separated form or -i. For batches >10
URLs, -i is recommended.

Exit codes: 0 ok | 1 bad args | 2 network (all retries exhausted) | 3 partial |
            4 cancelled.
`, download.Version, download.Version)
}
