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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"odm/internal/config"
	"odm/internal/download"
	"odm/internal/logging"
	"odm/internal/ratelimit"
	"odm/internal/rpc"
	"odm/internal/scheduler"
	"odm/internal/transport"
	"odm/internal/ui"
	"odm/internal/version"
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

	o, _, err := config.Setup(argv)
	if err != nil {
		if err == pflag.ErrHelp {
			printUsage(os.Stdout)
			return nil
		}
		// A flag/validation error is one line: the message plus a pointer to
		// `-h` for the full syntax. Dumping the entire usage table here drowns
		// the actual error (and the message already names the offending flags).
		return errExit{code: download.ExitGeneral,
			msg: err.Error() + " (run 'odm -h' for usage)"}
	}

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
	if o.OutFile != "" && len(o.URLs) > 1 {
		return errExit{code: download.ExitGeneral,
			msg: "--output/-o is only valid with a single URL (every task would write the same file)"}
	}

	exec, err := buildExecOptions(o)
	if err != nil {
		return err
	}
	// Checksum verification is per-task (the engine hashes the actual written
	// file). With multiple URLs there is one --checksum flag but many files, so
	// the option only makes sense in single-file mode — clear it so a batch never
	// hashes every file against the same digest, and tell the user it was dropped
	// (before the confirmation prompt / any UI renderer; quiet sessions keep
	// stderr clean for cron/scripts).
	if len(o.URLs) > 1 {
		if w := batchChecksumWarning(o); w != "" {
			fmt.Fprintln(os.Stderr, w)
		}
		exec.Checksum = ""
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
	probes := make(map[string]*transport.ProbeResult, len(o.URLs))
	// profiles holds the concrete engine per URL: the user's --profile for
	// odm/aria2c/both, or the smart decision resolved after the Balancer pass
	// (which fixes the per-file connection budget). It feeds the §9 batch prompt
	// (which shows each file's engine) and is injected into each Task via
	// SetProfile so Start doesn't re-probe h2 readiness. Populated below only
	// for smart; explicit profiles need no per-file map (empty is fine).
	// profileReasons carries ChooseProfile's why (e.g. "no h2") for the prompt.
	profiles := make(map[string]string, len(o.URLs))
	profileReasons := make(map[string]string, len(o.URLs))
	probeClient := mgr.Client()
	// Probe up to 8 URLs in parallel: N URLs × 15s serial timeouts would stall
	// a long -i batch before a single download starts. Each probe gets its own
	// timeout so a slow/unresponsive server can't block the whole batch. On
	// failure we fall back to sizeless single-stream (the Balancer gives it
	// 1 connection and escalates to single-stream).
	type probeResult struct {
		url string
		pr  *transport.ProbeResult
		err error
	}
	workers := min(len(o.URLs), 8)
	if workers < 1 {
		workers = 1
	}
	probeCh := make(chan probeResult, len(o.URLs))
	for w := 0; w < workers; w++ {
		go func(w int) {
			// Stride the URL list across workers so each URL is probed exactly
			// once. Probing every URL in every worker duplicates results, and
			// the consumer below then fills the batch with the first N results
			// — the same URL twice, or missing URLs entirely when N > workers.
			for i := w; i < len(o.URLs); i += workers {
				u := o.URLs[i]
				probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
				pr, perr := probeClient.Probe(probeCtx, u)
				probeCancel()
				// Honour ctx cancellation: a ^C during the probe pass must not
				// leave this goroutine blocked on the send (nothing consumes
				// probeCh once run returns). probeCtx's parent is ctx, so the
				// in-flight Probe aborts promptly too.
				select {
				case probeCh <- probeResult{url: u, pr: pr, err: perr}:
				case <-ctx.Done():
					return
				}
			}
		}(w)
	}
	for range o.URLs {
		// The consumer must mirror the workers' ctx awareness: if the probe
		// pass is cancelled mid-way, fewer than len(o.URLs) results arrive and
		// a plain receive would block forever.
		var res probeResult
		select {
		case res = <-probeCh:
		case <-ctx.Done():
			return errExit{code: download.ExitCancelled, msg: "cancelled during probe"}
		}
		if res.err != nil {
			logger.Warnf("probe failed for %s: %v", res.url, res.err)
			files = append(files, scheduler.FileInput{URL: res.url, SupportsRange: false})
			sizes[res.url] = -1
			continue
		}
		files = append(files, scheduler.FileInput{URL: res.url, SupportsRange: res.pr.SupportsRange})
		sizes[res.url] = res.pr.TotalSize
		probes[res.url] = res.pr
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].URL < files[j].URL })

	plan, err := scheduler.Compute(o.Connections, files, o.SplitFile, o.MaxConnection)
	if err != nil {
		return errExit{code: download.ExitGeneral, msg: err.Error()}
	}
	if plan.Warning != "" {
		logger.Warnf("%s", plan.Warning)
	}
	// Resolve the concrete engine per file, now that the Balancer has decided
	// the real per-file connection budget (plan.Parallel[i].Connections — not a
	// guess from the CLI flags). For smart this is the decision matrix (range
	// support, size, h2 readiness, budget); for explicit profiles it's the flag.
	// Resolved once here so the prompt shows it and every task skips the
	// re-resolution (and its extra h2 HEAD probe) in Start.
	if o.Profile == "smart" {
		profiles = make(map[string]string, len(plan.Parallel)+len(plan.Queued))
		for _, a := range append(append([]scheduler.Allocation{}, plan.Parallel...), plan.Queued...) {
			if pr := probes[a.URL]; pr != nil {
				p, reason := resolveProfile(o, pr, a.Connections, mgr.H2Client())
				profiles[a.URL] = p
				if reason != "" {
					profileReasons[a.URL] = reason
				}
			}
		}
	}

	// §9 confirmation prompt — skipped when -y/--yes OR --quiet (PRD §9).
	if !o.Yes && !o.Quiet {
		ok, err := confirmPlan(o, plan, sizes, profiles, profileReasons, mgr)
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

	// TaskMaker that reuses the probe done above (the CLI already probed every
	// URL for the Balancer), so each Task.Start doesn't probe a second time.
	// URLs whose probe failed are left without a pre-probe — the task probes
	// itself when it starts.
	maker := func(url string, idx int) (*download.Task, int, error) {
		t, conns, err := mgr.NewTask(url, idx)
		if err == nil {
			if pr := probes[url]; pr != nil {
				t.SetProbe(pr)
			}
			if p := profiles[url]; p != "" {
				// Pin the resolved engine so Start skips the smart re-resolution
				// (and its extra h2 HEAD probe) — the decision was already made
				// once, up front, with the same inputs.
				t.SetProfile(p)
			}
		}
		return t, conns, err
	}

	sch := scheduler.NewScheduler(plan, maker, progCB)
	uiCtx, uiCancel := context.WithCancel(context.Background())
	defer uiCancel()
	// uiDone guards the final frame: RunLoop renders one last Frame on cancel,
	// so main must wait for it to exit BEFORE issuing its own Frame(nil,nil) —
	// otherwise two goroutines write the same stdout concurrently and the
	// cursor-up/clear sequences interleave into the doubled/truncated lines
	// seen on ^C.
	uiDone := make(chan struct{})
	go r.RunLoop(uiCtx, 100*time.Millisecond, snap, qSnap, uiDone)

	// Wake the redraw loop on SIGWINCH so a terminal resize is reflected on
	// the very next frame instead of waiting out the 100ms tick (whose
	// timeout we'd otherwise burn sleeping inside a resize storm).
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			select {
			case r.Wake <- struct{}{}:
			default: // coalesce bursts; the sized renderer already re-reads per frame
			}
		}
	}()

	succeeded, failed, runErr := sch.Run(ctx)
	uiCancel()
	// RunLoop does NOT render a final frame on cancel (see its ctx.Done case);
	// it only restores the cursor and closes uiDone. Waiting guarantees the
	// loop is fully gone before THIS goroutine issues the single final frame —
	// exactly one writer of the post-run screen, so the task lines can't
	// double or interleave.
	<-uiDone
	// Final frame so the terminal lands on the completed/error bars.
	r.Frame(nil, nil)

	// §13 exit-code mapping. A user-initiated ^C / SIGTERM is "cancelled" (4),
	// not a network/partial failure: the scheduler returns context.Canceled and
	// the in-flight tasks' partial bytes are preserved for --continue.
	code := download.ExitCodeFrom(succeeded, failed, 0)
	if runErr == context.Canceled {
		code = download.ExitCancelled
	}
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
			fmt.Println(version.Version)
			return true
		}
	}
	return false
}

// batchChecksumWarning returns the warning to print when --checksum is dropped
// for a multi-URL batch (one hash cannot cover many files), or "" when no
// warning is warranted: single-URL runs, no checksum given, or --quiet sessions
// (where stderr should stay clean for cron/scripts). It is deliberately a pure
// function of *config.Options so the drop branch is unit-testable without a
// network round-trip.
func batchChecksumWarning(o *config.Options) string {
	if len(o.URLs) > 1 && o.Checksum != "" && !o.Quiet {
		return "warning: --checksum ignored when downloading multiple URLs (one hash cannot cover multiple files)"
	}
	return ""
}

// resolveProfile picks the concrete engine for one URL, given the Balancer's
// real per-file connection budget. For the smart profile this is the decision
// matrix (range support, size, h2 readiness, budget); for the explicit
// profiles it's just the flag. The reason (from ChooseProfile's matrix) lets
// the prompt explain why a file landed on a given engine. The result is
// injected into each task via SetProfile so Start doesn't re-resolve (and
// re-probe h2) per file.
func resolveProfile(o *config.Options, pr *transport.ProbeResult, conns int, h2 *transport.Client) (profile, reason string) {
	if o.Profile != "smart" {
		return o.Profile, ""
	}
	if pr == nil {
		return "odm", "probe failed"
	}
	h2Ready := false
	if h2 != nil {
		h2Ready = h2.SupportsHTTP2(context.Background(), pr.FinalURL)
	}
	profile, reason = download.ChooseProfile(download.ServerCapabilities{
		TotalSize:     pr.TotalSize,
		SupportsRange: pr.SupportsRange,
		SingleStream:  pr.SingleStream,
		HTTP2Ready:    h2Ready,
		Conns:         conns,
	})
	return profile, reason
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
	minSplit, err := parseChunkSize(o.MinSplitSize)
	if err != nil {
		return download.ExecOptions{}, fmt.Errorf("--min-split-size %q: %w", o.MinSplitSize, err)
	}
	return download.ExecOptions{
		Dir:              dir,
		OutFile:          o.OutFile,
		Connections:      o.Connections,
		MaxConn:          o.MaxConnection,
		SplitFile:        o.SplitFile,
		Retry:            o.Retry,
		RetryWait:        time.Duration(o.RetryWait) * time.Second,
		Continue:         o.Continue,
		ChunkSize:        chunk,
		Timeout:          time.Duration(o.Timeout) * time.Second,
		Checksum:         o.Checksum,
		LimitRate:        o.LimitRate,
		TaskLimitRate:    o.TaskLimitRate,
		UserAgent:        o.UserAgent,
		Headers:          o.Headers,
		Referer:          o.Referer,
		Proxy:            o.Proxy,
		CheckCert:        o.CheckCertificate,
		MaxRedirect:      o.MaxRedirect,
		Profile:          o.Profile,
		Split:            o.Split,
		MinSplitSize:     minSplit,
		MaxConnPerServer: o.MaxConnPerServer,
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
func confirmPlan(o *config.Options, plan *scheduler.Plan, sizes map[string]int64, profiles map[string]string, reasons map[string]string, mgr *download.Manager) (bool, error) {
	useColor := ui.IsTTY(os.Stdout)
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
		return ui.ConfirmSingle(os.Stdin, os.Stdout, disp, name, size, conns, useColor)
	}
	// For the smart profile the per-file engine IS the interesting info — show
	// it even when it resolves to the default odm engine. Explicit profiles
	// (odm/aria2c/both) need no per-file tag.
	rows := ui.RowsFromPlan(plan, sizes, profiles, reasons, o.Profile == "smart")
	// The per-file budget shown is the MODE's base, not one file's count: in
	// Mode C (-sf) every parallel file gets SF, with a remainder top-up on the
	// first ones and single-stream files capped at 1 — so Parallel[0].Connections
	// can be SF+1 and a non-range file 1, neither of which represents the batch.
	// Showing SF (the mode's base) is honest and matches what queued files get.
	connsPerFile := 1
	if o.SplitFile > 0 {
		connsPerFile = o.SplitFile // Mode C base per file
	} else if len(plan.Parallel) > 0 {
		connsPerFile = plan.Parallel[0].Connections // Mode A/B: 1 or the budget
	}
	return ui.ConfirmBatch(os.Stdin, os.Stdout, rows, connsPerFile, len(plan.Parallel), len(o.URLs), useColor)
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
//
// The returned cancel also stops the signal handler, so a call to cancel()
// (the deferred `defer cancel()` in run/runRPC, or the signal path itself)
// releases the goroutine: it exits on the first signal and the handler is
// deregistered, leaving no goroutine parked on the signal channel after the
// program has moved on.
func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
			// Program moved on (deferred cancel) — nothing to do; the handler
			// is stopped below.
		}
	}()
	return ctx, func() {
		signal.Stop(sig)
		cancel()
	}
}

// runRPC starts the JSON-RPC + WebSocket server over an empty scheduler daemon
// and serves until odm.shutdown arrives or the process is signalled.
func runRPC(o *config.Options, engineLog download.LogFn, logger *logging.Logger) error {
	exec, err := buildExecOptions(o)
	if err != nil {
		return err
	}
	// The -o single-file override only makes sense for the one-shot CLI path
	// (the CLI itself rejects it for multiple URLs). In daemon mode every
	// addUri task would otherwise write to the SAME OutFile — overlapping
	// concurrent WriteAt calls corrupt the destination silently (see
	// storage.File.WriteAt's disjoint-range contract). Clear it so each task
	// derives its own name from its URL.
	exec.OutFile = ""
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
	progTh := rpc.NewProgressThrottler(bc)
	sch := scheduler.NewEmptyScheduler(slots, mgr.NewTask, progTh.Forward)
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
	useTLS := o.RPCTLSCert != "" && o.RPCTLSKey != ""
	if useTLS {
		logger.Infof("odm RPC listening on https://%s (secret:%s, tls:%s)", addr, secretWord(o.RPCSecret), o.RPCTLSCert)
	} else {
		logger.Infof("odm RPC listening on http://%s (secret:%s)", addr, secretWord(o.RPCSecret))
	}
	serveErr := make(chan error, 1)
	go func() {
		if useTLS {
			serveErr <- http.ServeTLS(ln, mux, o.RPCTLSCert, o.RPCTLSKey)
		} else {
			serveErr <- http.Serve(ln, mux)
		}
	}()

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
  -m, --max-connections  ceiling; exceeding it just warns     (default 32)
      --split-file/-sf  connections per file in batch mode   (unset = 1 per file)

Inputs / output:
  -o, --output NAME   output filename (single-file only)
  -d, --dir PATH       destination directory                  (default cwd)
  -i, --input-file FILE  read URL list from FILE

Behavior:
  -y, --yes           skip the confirmation prompt
  -q, --quiet         no progress bar (cron/scripts); also skips the prompt
  -x, --continue      resume an incomplete file via the .odm control file (default on)
  -s, --chunk-size SIZE   work-stealing chunk size, e.g. 4M   (default 4M)

Engine profiles (--profile aria2c|both|smart):
      --profile NAME  engine profile: odm (default) | aria2c | both | smart
      --split N       aria2c: number of segments per file                (default 5)
      --min-split-size SIZE   aria2c: don't split ranges < 2x this size  (default 20M)
      --max-connection-per-server N   aria2c: cap per server in h1       (default 1)

HTTP / network:
  -n, --max-redirect N   redirect hops to follow          (default 5)
  -r, --retry N          retries per segment on failure    (default 3)
  -w, --retry-wait SEC   delay between retries            (default 2)
  -t, --timeout SEC      dial+headers timeout             (default 30)
  -u, --user-agent UA    custom User-Agent               (default %s)
  -H, --header K:V       add a custom header (repeatable)
      --referer URL      set the Referer header
  -p, --proxy URL        http/https/socks5 proxy
      --check-cert BOOL  verify TLS                       (default true)
      --checksum algo:hash  verify md5/sha1/sha256
  -l, --limit-rate RATE   global speed limit, e.g. 5M/500K
      --limit-rate-per-task RATE  per-task speed cap (stacked on global), e.g. 2M

Config / logging / RPC:
      --config PATH     config file path               (default /etc/odm/config.conf)
  -L, --log FILE        mirror logs to FILE
      --log-level LEVEL debug|info|warn|error          (default info)
      --rpc             run as the RPC server (daemon)
      --rpc-listen-port PORT                           (default 6900)
      --rpc-listen-all  bind 0.0.0.0 (default 127.0.0.1)
      --rpc-secret TOKEN  auth for RPC ('token:<secret>' param or ?secret=)
      --rpc-tls-cert FILE  TLS certificate (PEM); enables HTTPS
      --rpc-tls-key FILE   TLS private key (PEM); requires --rpc-tls-cert
  -V, --version         print version and exit
  -h, --help            show this help

Config priority: CLI > ~/.config/odm/config.conf > /etc/odm/config.conf > defaults.
URLs with a literal comma must use space-separated form or -i. For batches >10
URLs, -i is recommended.

Exit codes: 0 ok | 1 bad args | 2 network (all retries exhausted) | 3 partial |
            4 cancelled.
`, version.Version, version.Version)
}
