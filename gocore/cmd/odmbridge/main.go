// Command odmbridge is the c-archive entry point for the Rust CLI's Go engine.
// It reuses gocore/internal exactly like the former Go CLI (config → probe →
// connection split → download.Task) and exposes only the three ABI symbols
// declared in crates/app/src/go_engine.rs.
package main

/*
#include <stdlib.h>
#include "cbshim.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"odm/internal/config"
	"odm/internal/download"
	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// --- config JSON -----------------------------------------------------------

// bridgeConfig mirrors the JSON object sent by the Rust bridge. Pointer fields
// distinguish "absent" (keep config.DefaultPtr's value) from an explicit zero.
type bridgeConfig struct {
	URLs                   []string  `json:"urls"`
	OutFile                *string   `json:"out_file"`
	Dir                    *string   `json:"dir"`
	Connections            *int      `json:"connections"`
	MaxConnections         *int      `json:"max_connections"`
	SplitFile              *int      `json:"split_file"`
	ChunkSize              *string   `json:"chunk_size"`
	Retry                  *int      `json:"retry"`
	RetryWait              *int      `json:"retry_wait"`
	Timeout                *int      `json:"timeout"`
	MaxRedirect            *int      `json:"max_redirect"`
	UserAgent              *string   `json:"user_agent"`
	Headers                *[]string `json:"headers"`
	LoadCookies            *string   `json:"load_cookies"`
	Referer                *string   `json:"referer"`
	Proxy                  *string   `json:"proxy"`
	CheckCertificate       *bool     `json:"check_certificate"`
	Checksum               *string   `json:"checksum"`
	ChecksumURL            *string   `json:"checksum_url"`
	Mirrors                *[]string `json:"mirrors"`
	LimitRate              *string   `json:"limit_rate"`
	LimitRatePerTask       *string   `json:"limit_rate_per_task"`
	AutoRename             *bool     `json:"auto_rename"`
	SkipExisting           *bool     `json:"skip_existing"`
	Continue               *bool     `json:"continue_"`
	Profile                *string   `json:"profile"`
	Split                  *int      `json:"split"`
	MinSplitSize           *string   `json:"min_split_size"`
	MaxConnectionPerServer *int      `json:"max_connection_per_server"`
	Quiet                  *bool     `json:"quiet"`
}

func addString(args *[]string, flag string, v *string) {
	if v != nil {
		*args = append(*args, "--"+flag+"="+*v)
	}
}

func addInt(args *[]string, flag string, v *int) {
	if v != nil {
		*args = append(*args, fmt.Sprintf("--%s=%d", flag, *v))
	}
}

func addBool(args *[]string, flag string, v *bool) {
	if v != nil {
		*args = append(*args, fmt.Sprintf("--%s=%t", flag, *v))
	}
}

func addArray(args *[]string, flag string, v *[]string) {
	if v != nil {
		for _, item := range *v {
			*args = append(*args, "--"+flag+"="+item)
		}
	}
}

// toArgs feeds the JSON object through config.Setup, so validation, defaults,
// the Netscape cookie loader and the flag-name mapping are the production ones.
func toArgs(c bridgeConfig) []string {
	var args []string
	addString(&args, "output", c.OutFile)
	addString(&args, "dir", c.Dir)
	addInt(&args, "connections", c.Connections)
	addInt(&args, "max-connections", c.MaxConnections)
	addInt(&args, "split-file", c.SplitFile)
	addString(&args, "chunk-size", c.ChunkSize)
	addInt(&args, "retry", c.Retry)
	addInt(&args, "retry-wait", c.RetryWait)
	addInt(&args, "timeout", c.Timeout)
	addInt(&args, "max-redirect", c.MaxRedirect)
	addString(&args, "user-agent", c.UserAgent)
	addArray(&args, "header", c.Headers)
	addString(&args, "load-cookies", c.LoadCookies)
	addString(&args, "referer", c.Referer)
	addString(&args, "proxy", c.Proxy)
	addBool(&args, "check-certificate", c.CheckCertificate)
	addString(&args, "checksum", c.Checksum)
	addString(&args, "checksum-url", c.ChecksumURL)
	addArray(&args, "mirror", c.Mirrors)
	addString(&args, "limit-rate", c.LimitRate)
	addString(&args, "limit-rate-per-task", c.LimitRatePerTask)
	addBool(&args, "auto-rename", c.AutoRename)
	addBool(&args, "skip-existing", c.SkipExisting)
	addBool(&args, "continue", c.Continue)
	addString(&args, "profile", c.Profile)
	addInt(&args, "split", c.Split)
	addString(&args, "min-split-size", c.MinSplitSize)
	addInt(&args, "max-connection-per-server", c.MaxConnectionPerServer)
	addBool(&args, "quiet", c.Quiet)
	args = append(args, "--")
	args = append(args, c.URLs...)
	return args
}

// parseChunkSize / buildExecOptions / collisionPolicy mirror cmd/odm/main.go,
// which was removed by the Rust port; the helpers it called live in internal.
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

func collisionPolicy(o *config.Options) string {
	switch {
	case o.AutoRename:
		return "rename"
	case o.SkipExisting:
		return "skip"
	default:
		return ""
	}
}

func buildExecOptions(o *config.Options) (download.ExecOptions, error) {
	chunk, err := parseChunkSize(o.ChunkSize)
	if err != nil {
		return download.ExecOptions{}, err
	}
	minSplit, err := parseChunkSize(o.MinSplitSize)
	if err != nil {
		return download.ExecOptions{}, fmt.Errorf("--min-split-size %q: %w", o.MinSplitSize, err)
	}
	dir := o.Dir
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			return download.ExecOptions{}, err
		}
	}
	exec := download.ExecOptions{
		Dir:              dir,
		OutFile:          o.OutFile,
		Connections:      o.Connections,
		SplitFile:        o.SplitFile,
		Retry:            o.Retry,
		RetryWait:        time.Duration(o.RetryWait) * time.Second,
		Continue:         o.Continue,
		ChunkSize:        chunk,
		Timeout:          time.Duration(o.Timeout) * time.Second,
		Checksum:         o.Checksum,
		ChecksumURL:      o.ChecksumURL,
		Mirrors:          o.Mirrors,
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
		Collision:        collisionPolicy(o),
	}
	// One --checksum cannot cover several different files (production drops it).
	if len(o.URLs) > 1 {
		exec.Checksum = ""
	}
	return exec, nil
}

func resolveProfile(o *config.Options, pr *transport.ProbeResult, conns int, h2 *transport.Client) string {
	if o.Profile != "smart" || pr == nil {
		return o.Profile
	}
	h2Ready := h2 != nil && h2.SupportsHTTP2(context.Background(), pr.FinalURL)
	profile, _ := download.ChooseProfile(download.ServerCapabilities{
		TotalSize:     pr.TotalSize,
		SupportsRange: pr.SupportsRange,
		SingleStream:  pr.SingleStream,
		HTTP2Ready:    h2Ready,
		Conns:         conns,
	})
	return profile
}

// perURLNames interprets a comma-separated out_file as one -o name per URL.
// A single URL keeps its name verbatim; without a matching list the engine
// derives names from each URL (production's multi-URL behaviour).
func perURLNames(raw string, n int) []string {
	if raw == "" {
		return nil
	}
	if n == 1 {
		return []string{raw}
	}
	parts := strings.Split(raw, ",")
	if len(parts) != n {
		return nil
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// distinctURLs gives duplicate URLs a harmless fragment suffix so two tasks
// can carry different per-URL names; fragments are never sent over HTTP.
func distinctURLs(urls []string) ([]string, map[string]string) {
	seen := map[string]bool{}
	out := make([]string, len(urls))
	display := make(map[string]string, len(urls))
	for i, u := range urls {
		if seen[u] {
			out[i] = fmt.Sprintf("%s#odm-%d", u, i)
		} else {
			seen[u] = true
			out[i] = u
		}
		display[out[i]] = u
	}
	return out, display
}

// --- connection split (the production Balancer, inlined) -------------------

type fileInput struct {
	url           string
	supportsRange bool
}

type allocation struct {
	url           string
	conns         int
	supportsRange bool
}

type batchPlan struct {
	parallel []allocation
	queued   []allocation
}

// computePlan mirrors scheduler.Compute's Mode A/B/C allocation: a single file
// gets the whole budget, a batch without split-file gives one connection per
// file and runs at most C files in parallel, and split-file gives SF per file
// with round-robin remainder and non-range files capped at one connection.
func computePlan(c int, files []fileInput, sf, maxConns int) (*batchPlan, error) {
	if maxConns <= 0 {
		maxConns = 32
	}
	n := len(files)
	if n == 0 {
		return nil, fmt.Errorf("no URLs provided")
	}
	if c < 1 {
		return nil, fmt.Errorf("connection budget (-c) must be at least 1")
	}
	if n == 1 && sf > 0 {
		sf = 0 // Mode A already claims the whole budget
	}
	if sf > c {
		return nil, fmt.Errorf("split-file (-sf) cannot be greater than the total connection budget (-c)")
	}
	p := &batchPlan{}
	switch {
	case n == 1:
		conns := min(c, maxConns)
		if !files[0].supportsRange {
			conns = 1
		}
		p.parallel = []allocation{{files[0].url, conns, files[0].supportsRange}}
	case sf == 0:
		slots := min(c, min(n, maxConns))
		for i, f := range files {
			a := allocation{f.url, 1, f.supportsRange}
			if i < slots {
				p.parallel = append(p.parallel, a)
			} else {
				p.queued = append(p.queued, a)
			}
		}
	default:
		parallelFiles := max(1, min(c/sf, min(n, maxConns)))
		conns := make([]int, parallelFiles)
		nonRange := make([]bool, parallelFiles)
		for i := range conns {
			conns[i] = sf
			nonRange[i] = !files[i].supportsRange
		}
		distribute(conns, c-parallelFiles*sf, nonRange)
		freed := 0
		for i := range conns {
			if nonRange[i] && conns[i] > 1 {
				freed += conns[i] - 1
				conns[i] = 1
			}
		}
		if freed > 0 {
			distribute(conns, freed, nonRange)
		}
		for i, f := range files {
			a := allocation{f.url, 1, f.supportsRange}
			if i < parallelFiles {
				a.conns = conns[i]
				p.parallel = append(p.parallel, a)
			} else {
				a.conns = sf
				if !f.supportsRange {
					a.conns = 1
				}
				p.queued = append(p.queued, a)
			}
		}
	}
	return p, nil
}

func distribute(conns []int, budget int, skip []bool) {
	if budget <= 0 || len(conns) == 0 {
		return
	}
	any := false
	for _, s := range skip {
		if !s {
			any = true
			break
		}
	}
	for budget > 0 && any {
		advanced := false
		for i := range conns {
			if skip[i] {
				continue
			}
			conns[i]++
			budget--
			advanced = true
			if budget == 0 {
				break
			}
		}
		if !advanced {
			break
		}
	}
}

// --- events and results ----------------------------------------------------

type progressEvent struct {
	Event   string  `json:"event"`
	ID      string  `json:"id"`
	URL     string  `json:"url"`
	File    string  `json:"file"`
	State   string  `json:"state"`
	Bytes   int64   `json:"bytes"`
	Total   int64   `json:"total"`
	Speed   float64 `json:"speed"`
	Conns   int     `json:"conns"`
	Errors  int     `json:"errors"`
	Retries int     `json:"retries"`
}

type resultTask struct {
	ID      string  `json:"id"`
	URL     string  `json:"url"`
	File    string  `json:"file"`
	State   string  `json:"state"`
	Bytes   int64   `json:"bytes"`
	Total   int64   `json:"total"`
	Speed   float64 `json:"speed"`
	Errors  int     `json:"errors"`
	Retries int     `json:"retries"`
	Error   string  `json:"error"`
}

type bridgeResult struct {
	Succeeded int          `json:"succeeded"`
	Failed    int          `json:"failed"`
	Tasks     []resultTask `json:"tasks"`
}

func stateName(s download.TaskState) string {
	switch s {
	case download.StateQueued:
		return "queued"
	case download.StateActive:
		return "active"
	case download.StatePaused:
		return "paused"
	case download.StateRetrying:
		return "retrying"
	case download.StateCompleted:
		return "completed"
	case download.StateError:
		return "error"
	default:
		return "unknown"
	}
}

func eventFrom(v download.ProgressView) progressEvent {
	return progressEvent{
		Event: "progress", ID: string(v.ID), URL: v.URL, File: v.Filename,
		State: stateName(v.State), Bytes: v.BytesDone, Total: v.TotalSize,
		Speed: float64(v.Speed), Conns: v.Connections, Errors: v.Errors, Retries: v.Retries,
	}
}

// emitter throttles live snapshots to one event per task every 250 ms (the
// cadence of rpc.ProgressThrottler) and emits completion events immediately.
type emitter struct {
	mu      sync.Mutex
	views   map[string]download.ProgressView
	results []resultTask
	display map[string]string
}

func newEmitter(display map[string]string) *emitter {
	return &emitter{views: map[string]download.ProgressView{}, display: display}
}

func (e *emitter) update(v download.ProgressView) {
	e.mu.Lock()
	e.views[string(v.ID)] = v
	e.mu.Unlock()
}

func (e *emitter) tick() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.views))
	for id := range e.views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	views := make([]download.ProgressView, 0, len(ids))
	for _, id := range ids {
		views = append(views, e.views[id])
	}
	e.mu.Unlock()
	for _, v := range views {
		sendProgress(eventFrom(v))
	}
}

func (e *emitter) final(v download.ProgressView, errText string) {
	if v.State == download.StateError && errText == "" {
		errText = "download failed"
	}
	if v.State == download.StatePaused && errText == "" {
		errText = "cancelled"
	}
	url := v.URL
	if orig, ok := e.display[url]; ok {
		url = orig
	}
	task := resultTask{
		ID: string(v.ID), URL: url, File: v.Filename,
		State: stateName(v.State), Bytes: v.BytesDone, Total: v.TotalSize,
		Speed: float64(v.Speed), Errors: v.Errors, Retries: v.Retries, Error: errText,
	}
	ev := eventFrom(v)
	ev.URL = url
	e.mu.Lock()
	delete(e.views, string(v.ID))
	e.results = append(e.results, task)
	e.mu.Unlock()
	sendProgress(ev)
}

func (e *emitter) result() bridgeResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	succeeded, failed := 0, 0
	for _, t := range e.results {
		if t.State == "completed" {
			succeeded++
		} else {
			failed++
		}
	}
	// Tasks left in views were never admitted (cancelled while queued).
	ids := make([]string, 0, len(e.views))
	for id := range e.views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v := e.views[id]
		url := v.URL
		if orig, ok := e.display[url]; ok {
			url = orig
		}
		e.results = append(e.results, resultTask{
			ID: id, URL: url, File: v.Filename, State: "error",
			Bytes: v.BytesDone, Total: v.TotalSize, Error: "task did not start",
		})
		failed++
	}
	e.views = map[string]download.ProgressView{}
	return bridgeResult{Succeeded: succeeded, Failed: failed, Tasks: append([]resultTask{}, e.results...)}
}

// --- C shim / callback -----------------------------------------------------

var (
	cbMu     sync.Mutex
	runMu    sync.Mutex
	cancelFn context.CancelFunc
	nextID   atomic.Int64
)

func sendProgress(ev progressEvent) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	cstr := C.CString(string(b))
	defer C.free(unsafe.Pointer(cstr))
	cbMu.Lock()
	C.odm_shim_progress(cstr)
	cbMu.Unlock()
}

//export odm_go_set_progress_callback
func odm_go_set_progress_callback(cb C.odm_progress_cb) {
	cbMu.Lock()
	C.odm_shim_set_progress(cb)
	cbMu.Unlock()
}

//export odm_go_cancel
func odm_go_cancel() {
	runMu.Lock()
	fn := cancelFn
	runMu.Unlock()
	if fn != nil {
		fn()
	}
}

// --- run -------------------------------------------------------------------

type taskMaker func(url string) (*download.Task, error)

func runBatch(ctx context.Context, plan *batchPlan, mk taskMaker, em *emitter, names map[string]string) {
	slots := len(plan.parallel)
	if slots < 1 {
		slots = 1
	}
	sem := make(chan struct{}, slots)
	var wg sync.WaitGroup
	launch := func(a allocation) {
		t, err := mk(a.url)
		if err != nil {
			<-sem // the caller reserved a slot before launch
			id := fmt.Sprintf("odm-%03d", nextID.Add(1))
			em.final(download.ProgressView{
				ID: download.TaskID(id), URL: a.url, Filename: names[a.url],
				State: download.StateError,
			}, err.Error())
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			startErr := t.Start(ctx, a.conns, em.update)
			v := t.Snapshot()
			if v.Filename == "" {
				v.Filename = names[a.url]
			}
			em.final(v, errString(startErr))
		}()
	}
	for _, a := range plan.parallel {
		sem <- struct{}{}
		launch(a)
	}
	for _, a := range plan.queued {
		select {
		case sem <- struct{}{}:
			launch(a)
		case <-ctx.Done():
			id := fmt.Sprintf("odm-%03d", nextID.Add(1))
			em.final(download.ProgressView{
				ID: download.TaskID(id), URL: a.url, Filename: names[a.url],
				State: download.StateError,
			}, "cancelled")
		}
	}
	wg.Wait()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func errorResult(res *bridgeResult, err error) {
	res.Failed = 1
	res.Tasks = append(res.Tasks, resultTask{State: "error", Error: err.Error()})
}

func run(ctx context.Context, cfg bridgeConfig, res *bridgeResult) int {
	o, _, err := config.Setup(toArgs(cfg))
	if err != nil {
		errorResult(res, err)
		return 1
	}
	if len(o.URLs) == 0 {
		errorResult(res, fmt.Errorf("no URLs provided"))
		return 1
	}

	names := perURLNames(o.OutFile, len(o.URLs))
	urls, display := distinctURLs(o.URLs)
	o.URLs = urls
	if len(names) > 0 || len(urls) > 1 {
		// Per-file names ride on the probe's Filename; a single -o rides on
		// ExecOptions.OutFile as in production.
		o.OutFile = ""
	}
	exec, err := buildExecOptions(o)
	if err != nil {
		errorResult(res, err)
		return 1
	}
	if len(names) == 1 {
		exec.OutFile = names[0]
	}
	nameByURL := map[string]string{}
	for i, u := range o.URLs {
		if i < len(names) {
			nameByURL[u] = names[i]
		}
	}

	mgr, err := download.NewManager(exec, nil)
	if err != nil {
		errorResult(res, err)
		return 1
	}

	// Probe every URL up front (the CLI path) so the balancer can give
	// non-range files one connection and progress events know the total.
	type probeResult struct {
		url string
		pr  *transport.ProbeResult
	}
	workers := min(len(o.URLs), 8)
	if workers < 1 {
		workers = 1
	}
	probeCh := make(chan probeResult, len(o.URLs))
	for w := 0; w < workers; w++ {
		go func(w int) {
			for i := w; i < len(o.URLs); i += workers {
				u := o.URLs[i]
				pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
				pr, perr := mgr.Client().Probe(pctx, u)
				pcancel()
				select {
				case probeCh <- probeResult{url: u, pr: pr}:
					_ = perr
				case <-ctx.Done():
					return
				}
			}
		}(w)
	}
	files := make([]fileInput, 0, len(o.URLs))
	probes := map[string]*transport.ProbeResult{}
	for range o.URLs {
		var pr probeResult
		select {
		case pr = <-probeCh:
		case <-ctx.Done():
			errorResult(res, ctx.Err())
			return 1
		}
		if pr.pr == nil {
			files = append(files, fileInput{pr.url, false})
			continue
		}
		files = append(files, fileInput{pr.url, pr.pr.SupportsRange})
		probes[pr.url] = pr.pr
	}

	plan, err := computePlan(o.Connections, files, o.SplitFile, o.MaxConnection)
	if err != nil {
		errorResult(res, err)
		return 1
	}
	profiles := map[string]string{}
	if o.Profile == "smart" {
		for _, a := range append(append([]allocation{}, plan.parallel...), plan.queued...) {
			if pr := probes[a.url]; pr != nil {
				profiles[a.url] = resolveProfile(o, pr, a.conns, mgr.H2Client())
			}
		}
	}

	em := newEmitter(display)
	// Same ~250 ms coalescing as rpc.ProgressThrottler: one event per task
	// per tick; completion events bypass the throttle via em.final.
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				em.tick()
			case <-ctx.Done():
				return
			}
		}
	}()

	mk := func(url string) (*download.Task, error) {
		id := download.TaskID(fmt.Sprintf("odm-%03d", nextID.Add(1)))
		profile := o.Profile
		if p := profiles[url]; p != "" {
			profile = p
		}
		// The engine's logger is silent here; Start's error is captured by
		// runBatch directly, which gives exact per-task error text.
		t := download.NewTask(id, url, download.TaskOptions{
			OutputName:       exec.OutFile,
			Dir:              exec.Dir,
			Retry:            exec.Retry,
			RetryWait:        exec.RetryWait,
			Continue:         exec.Continue,
			ChunkSize:        exec.ChunkSize,
			Timeout:          exec.Timeout,
			UserAgent:        exec.UserAgent,
			Checksum:         exec.Checksum,
			ChecksumURL:      exec.ChecksumURL,
			Mirrors:          exec.Mirrors,
			TaskLimitRate:    exec.TaskLimitRate,
			Profile:          profile,
			Split:            exec.Split,
			MinSplitSize:     exec.MinSplitSize,
			MaxConnPerServer: exec.MaxConnPerServer,
			Collision:        exec.Collision,
		}, mgr.Client(), mgr.Limiter(), nil)
		t.SetH2Client(mgr.H2Client())
		if pr := probes[url]; pr != nil {
			pr2 := *pr
			if name := nameByURL[url]; name != "" {
				pr2.Filename = name
			}
			t.SetProbe(&pr2)
		}
		if p := profiles[url]; p != "" {
			t.SetProfile(p)
		}
		return t, nil
	}

	runBatch(ctx, plan, mk, em, nameByURL)
	*res = em.result()
	if res.Succeeded == 0 && res.Failed == 0 {
		return 1
	}
	if res.Failed > 0 {
		return 1
	}
	return 0
}

//export odm_go_run
func odm_go_run(configJSON *C.char, resultBuf *C.char, resultLen C.int) C.int {
	res := bridgeResult{Tasks: []resultTask{}}
	rc := C.int(1)
	defer func() {
		b, err := json.Marshal(res)
		if err != nil || resultBuf == nil || resultLen <= 0 {
			return
		}
		n := len(b)
		if n >= int(resultLen) {
			n = int(resultLen) - 1
		}
		buf := unsafe.Slice((*byte)(unsafe.Pointer(resultBuf)), int(resultLen))
		copy(buf[:n], b[:n])
		buf[n] = 0
	}()
	if configJSON == nil {
		errorResult(&res, fmt.Errorf("config_json is NULL"))
		return rc
	}
	var cfg bridgeConfig
	if err := json.Unmarshal([]byte(C.GoString(configJSON)), &cfg); err != nil {
		errorResult(&res, fmt.Errorf("config_json: %w", err))
		return rc
	}
	ctx, cancel := context.WithCancel(context.Background())
	runMu.Lock()
	cancelFn = cancel
	runMu.Unlock()
	defer func() {
		cancel()
		runMu.Lock()
		cancelFn = nil
		runMu.Unlock()
	}()
	rc = C.int(run(ctx, cfg, &res))
	return rc
}

func main() {}
