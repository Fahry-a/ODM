package rpc

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"odm/internal/download"
	"odm/internal/scheduler"
	"odm/internal/version"
)

// Server is the JSON-RPC 2.0 + WebSocket RPC surface. It owns a
// scheduler.Daemon (which owns the live Scheduler + Manager) and a WebSocket
// Broadcaster for event notifications.
type Server struct {
	daemon *scheduler.Daemon
	bc     *Broadcaster
	secret string // "" ⇒ no auth required (still strongly discouraged to bind to 0.0.0.0 then)
}

// NewServerWithBroadcaster is like NewServer but adopts a pre-built Broadcaster
// rather than allocating its own. Used by runRPC to share one Broadcaster between
// the scheduler's progress callback (set up before the Daemon) and the Server's
// own emissions (addUri start/pause, completion/error).
func NewServerWithBroadcaster(daemon *scheduler.Daemon, secret string, bc *Broadcaster) *Server {
	if bc == nil {
		bc = NewBroadcaster()
	}
	return &Server{daemon: daemon, bc: bc, secret: secret}
}

// OnTaskComplete maps a task's terminal snapshot onto the matching
// WebSocket event and fans it out: onDownloadComplete for a finished task,
// onDownloadError for a failed/cancelled one. Wired onto the Daemon's
// OnComplete hook by runRPC so handleComplete triggers it as each task leaves
// the live set. Safe to call from any goroutine (Broadcaster.Broadcast is
// mutex-guarded); invoked off the scheduler's completion path.
func (s *Server) OnTaskComplete(v download.ProgressView) {
	if v.State == download.StateCompleted {
		s.bc.Broadcast(Event{Method: "onDownloadComplete", Params: dbSnapshot(v)})
		return
	}
	// Any non-completed terminal state (error/cancelled) reads as an error event.
	s.bc.Broadcast(Event{Method: "onDownloadError", Params: dbSnapshot(v)})
}

// Broadcaster exposes the event fan-out (the engine emits via it; tests assert).
func (s *Server) Broadcaster() *Broadcaster { return s.bc }

// Routes installs /rpc and /ws on an *http.ServeMux. The caller binds the mux
// with http.ListenAndServe on the configured host:port.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/rpc", s.handleRPC)
	mux.HandleFunc("/ws", s.handleWS)
}

// Daemon returns the underlying daemon (testing/inspection).
func (s *Server) Daemon() *scheduler.Daemon { return s.daemon }

// --- auth ------------------------------------------------------------------

// checkSecret enforces aria2-style `token:<secret>` first-param auth.
// When s.secret is empty, auth is disabled (we still only bind 127.0.0.1 by
// default per). Returns true on match/when-no-secret-configured.
func (s *Server) checkSecret(req *jsonRPCRequest) bool {
	if s.secret == "" {
		return true
	}
	if len(req.Params) == 0 {
		return false
	}
	first, ok := req.Params[0].(string)
	if !ok {
		return false
	}
	// Constant-time compare: a plain == leaks the secret one byte at a time
	// through response timing on a network-exposed daemon (--rpc-listen-all).
	return strings.HasPrefix(first, "token:") &&
		subtle.ConstantTimeCompare([]byte(first[6:]), []byte(s.secret)) == 1
}

// --- HTTP dispatch ---------------------------------------------------------

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.checkQuerySecret(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.bc.addWS(w, r); err != nil {
		http.Error(w, "ws upgrade failed: "+err.Error(), http.StatusInternalServerError)
	}
}

// checkQuerySecret authenticates the WebSocket upgrade against ?secret=<value>
// when a secret is configured (the framed `token:` mechanism doesn't fit a
// browser opening ws://... directly). Constant-time compare, same reason as
// checkSecret.
func (s *Server) checkQuerySecret(r *http.Request) bool {
	if s.secret == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("secret")), []byte(s.secret)) == 1
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, nil, codeParseError, "parse: "+err.Error())
		return
	}

	// JSON-RPC allows a batch (array) request; we support both single and batch.
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var batch []jsonRPCRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			writeErr(w, nil, codeParseError, "batch parse: "+err.Error())
			return
		}
		shutdown := false
		resp := make([]jsonRPCResponse, 0, len(batch))
		for _, q := range batch {
			if q.Method == "odm.shutdown" {
				shutdown = true
			}
			resp = append(resp, s.dispatch(q))
		}
		writeJSON(w, resp)
		s.stopAfterWrite(w, shutdown)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, nil, codeParseError, "parse: "+err.Error())
		return
	}
	writeJSON(w, s.dispatch(req))
	s.stopAfterWrite(w, req.Method == "odm.shutdown")
}

// stopAfterWrite runs the daemon shutdown only after the JSON-RPC response has
// been written and flushed. Stop cancels the scheduler and — via the Daemon's
// OnDead hook — closes the listener in runRPC, which makes the process exit; if
// that happened before the write, the "OK" response would be truncated.
func (s *Server) stopAfterWrite(w http.ResponseWriter, shutdown bool) {
	if !shutdown {
		return
	}
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.Flush()
	}
	s.daemon.Stop()
}

// dispatch routes a single JSON-RPC request through the method table.
func (s *Server) dispatch(req jsonRPCRequest) jsonRPCResponse {
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
	if !s.checkSecret(&req) {
		resp.Error = &rpcError{Code: codeAuthError, Message: "bad or missing secret"}
		return resp
	}
	switch req.Method {
	case "odm.addUri":
		return s.methodAddURI(&req, &resp)
	case "odm.addBatch":
		return s.methodAddBatch(&req, &resp)
	case "odm.pause", "odm.pauseAll":
		return s.methodPause(&req, &resp, req.Method == "odm.pauseAll")
	case "odm.unpause", "odm.unpauseAll":
		return s.methodUnpause(&req, &resp, req.Method == "odm.unpauseAll")
	case "odm.remove":
		return s.methodRemove(&req, &resp)
	case "odm.tellStatus":
		return s.methodTellStatus(&req, &resp)
	case "odm.tellActive":
		resp.Result = dbSnapshots(s.daemon.TellActive())
	case "odm.tellWaiting":
		resp.Result = dbSnapshots(s.daemon.TellWaiting())
	case "odm.tellStopped":
		resp.Result = dbSnapshots(s.daemon.TellStopped())
	case "odm.changeOption":
		return s.methodChangeOption(&req, &resp)
	case "odm.getGlobalStat":
		resp.Result = map[string]any{
			"active":  len(s.daemon.TellActive()),
			"waiting": len(s.daemon.TellWaiting()),
			"stopped": len(s.daemon.TellStopped()),
		}
	case "odm.getVersion":
		resp.Result = map[string]any{
			"version": version.Version,
			"enabledFeatures": []string{"Multi-Connection", "Range-Download",
				"Resume", "Batch", "Checksum", "RateLimit-Agent", "TLS"},
		}
	case "odm.shutdown":
		resp.Result = "OK"
		// The actual daemon stop happens in stopAfterWrite, after handleRPC has
		// flushed this response, so a client never sees a truncated reply.
	case "odm.noop":
		resp.Result = "OK"
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
	return resp
}

// --- parameter extraction helpers -----------------------------------------

// stripToken removes a leading "token:..." first param (if present) and returns
// the remaining params. Auth already passed in checkSecret; this only strips the
// secret-bearing param so method handlers see their real arguments. Crucially,
// the first param is only dropped when it actually looks like a token (starts
// with "token:") — otherwise an ordinary string arg (a URL, a task goid, …)
// would be wrongly consumed.
func stripToken(p []any) []any {
	if len(p) > 0 {
		if s, ok := p[0].(string); ok && strings.HasPrefix(s, "token:") {
			return p[1:]
		}
	}
	return p
}

func strParam(p []any, idx int) (string, error) {
	if idx >= len(p) {
		return "", fmt.Errorf("missing param %d", idx)
	}
	if v, ok := p[idx].(string); ok {
		return v, nil
	}
	return "", fmt.Errorf("param %d not a string", idx)
}

func intParam(p []any, idx int) (int, error) {
	if idx >= len(p) {
		return 0, fmt.Errorf("missing param %d", idx)
	}
	switch v := p[idx].(type) {
	case float64:
		return int(v), nil
	case int:
		return v, nil
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("not an integer: %q", v)
		}
		return i, nil
	}
	return 0, fmt.Errorf("param %d not a number", idx)
}

// --- methods --------------------------------------------------------------

func (s *Server) methodAddURI(req *jsonRPCRequest, resp *jsonRPCResponse) jsonRPCResponse {
	p := stripToken(req.Params)
	url, err := strParam(p, 0)
	if err != nil {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: err.Error()}
		return *resp
	}
	conns, _ := intParam(p, 1)
	id, err := s.daemon.AddURL(url, conns)
	if err != nil {
		resp.Error = &rpcError{Code: codeInternalError, Message: err.Error()}
		return *resp
	}
	s.bc.Broadcast(Event{Method: "onDownloadStart", Params: map[string]string{"goid": string(id)}})
	resp.Result = string(id)
	return *resp
}

func (s *Server) methodAddBatch(req *jsonRPCRequest, resp *jsonRPCResponse) jsonRPCResponse {
	p := stripToken(req.Params)
	if len(p) == 0 {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: "no urls"}
		return *resp
	}
	urlsAny, ok := p[0].([]any)
	if !ok {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: "params[0] must be a url list"}
		return *resp
	}
	conns, _ := intParam(p, 1)
	ids := make([]string, 0, len(urlsAny))
	for _, u := range urlsAny {
		url, ok := u.(string)
		if !ok {
			continue
		}
		id, err := s.daemon.AddURL(url, conns)
		if err != nil {
			continue
		}
		ids = append(ids, string(id))
	}
	resp.Result = ids
	return *resp
}

func (s *Server) methodPause(req *jsonRPCRequest, resp *jsonRPCResponse, all bool) jsonRPCResponse {
	if all {
		for _, v := range s.daemon.TellActive() {
			s.daemon.Pause(v.ID)
		}
		resp.Result = "OK"
		return *resp
	}
	p := stripToken(req.Params)
	id, err := strParam(p, 0)
	if err != nil {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: err.Error()}
		return *resp
	}
	if s.daemon.Pause(download.TaskID(id)) {
		s.bc.Broadcast(Event{Method: "onDownloadPause", Params: map[string]string{"goid": id}})
		resp.Result = "OK"
	} else {
		resp.Error = &rpcError{Code: codeInternalError, Message: "task not found"}
	}
	return *resp
}

func (s *Server) methodUnpause(req *jsonRPCRequest, resp *jsonRPCResponse, all bool) jsonRPCResponse {
	if all {
		for _, v := range s.daemon.TellActive() {
			s.daemon.Unpause(v.ID)
		}
		resp.Result = "OK"
		return *resp
	}
	p := stripToken(req.Params)
	id, err := strParam(p, 0)
	if err != nil {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: err.Error()}
		return *resp
	}
	if s.daemon.Unpause(download.TaskID(id)) {
		resp.Result = "OK"
	} else {
		resp.Error = &rpcError{Code: codeInternalError, Message: "task not found"}
	}
	return *resp
}

func (s *Server) methodRemove(req *jsonRPCRequest, resp *jsonRPCResponse) jsonRPCResponse {
	p := stripToken(req.Params)
	id, err := strParam(p, 0)
	if err != nil {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: err.Error()}
		return *resp
	}
	if s.daemon.Remove(download.TaskID(id)) {
		resp.Result = "OK"
	} else {
		resp.Error = &rpcError{Code: codeInternalError, Message: "task not found"}
	}
	return *resp
}

func (s *Server) methodTellStatus(req *jsonRPCRequest, resp *jsonRPCResponse) jsonRPCResponse {
	p := stripToken(req.Params)
	id, err := strParam(p, 0)
	if err != nil {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: err.Error()}
		return *resp
	}
	if v, ok := s.daemon.TellStatus(download.TaskID(id)); ok {
		resp.Result = dbSnapshot(v)
	} else {
		resp.Error = &rpcError{Code: codeInternalError, Message: "task not found"}
	}
	return *resp
}

func (s *Server) methodChangeOption(req *jsonRPCRequest, resp *jsonRPCResponse) jsonRPCResponse {
	p := stripToken(req.Params)
	if len(p) < 2 {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: "changeOption needs gid and options dict"}
		return *resp
	}
	gid, ok := p[0].(string)
	if !ok {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: "gid must be a string"}
		return *resp
	}
	opts, ok := p[1].(map[string]any)
	if !ok {
		resp.Error = &rpcError{Code: codeInvalidParams, Message: "options must be a dict"}
		return *resp
	}
	for k, v := range opts {
		switch k {
		case "max-download-limit":
			val, ok := v.(string)
			if !ok {
				resp.Error = &rpcError{Code: codeInvalidParams, Message: "max-download-limit must be a string"}
				return *resp
			}
			if err := s.daemon.ChangeGlobalLimit(val); err != nil {
				resp.Error = &rpcError{Code: codeInternalError, Message: err.Error()}
				return *resp
			}
		case "max-download-limit-per-task":
			val, ok := v.(string)
			if !ok {
				resp.Error = &rpcError{Code: codeInvalidParams, Message: "max-download-limit-per-task must be a string"}
				return *resp
			}
			if !s.daemon.ChangeTaskLimit(download.TaskID(gid), val) {
				resp.Error = &rpcError{Code: codeInternalError, Message: "task not found"}
				return *resp
			}
		case "connections":
			nc, err := intParam([]any{v}, 0)
			if err != nil {
				resp.Error = &rpcError{Code: codeInvalidParams, Message: "connections must be an integer"}
				return *resp
			}
			s.daemon.ChangeConns(download.TaskID(gid), nc)
		}
	}
	resp.Result = "OK"
	return *resp
}

// --- response shaping ------------------------------------------------------

// dbSnapshot converts an internal ProgressView into the JSON shape GUIs expect
// ( flavours mirror aria2's tellStatus field names so AriaNg-style GUIs adapt
// easily).
func dbSnapshot(v download.ProgressView) map[string]any {
	return map[string]any{
		"goid":         string(v.ID),
		"url":          v.URL,
		"finalUrl":     v.FinalURL,
		"filename":     v.Filename,
		"status":       v.State.String(),
		"totalSize":    v.TotalSize,
		"bytesDone":    v.BytesDone,
		"speed":        v.Speed,
		"connections":  v.Connections,
		"etaSeconds":   int(v.ETA.Seconds()),
		"errors":       v.Errors,
		"retries":      v.Retries,
		"singleStream": v.SingleStream,
	}
}

func dbSnapshots(vs []download.ProgressView) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, dbSnapshot(v))
	}
	return out
}

// --- io -------------------------------------------------------------------

// SnapshotParams is the exported form of dbSnapshot: the field set carried in
// every WebSocket event (onDownloadProgress/Start/Complete/Error). Callers
// outside the rpc package (runRPC's progress throttler) build their own events
// and need the same JSON shape as the method handlers' responses.
func SnapshotParams(v download.ProgressView) map[string]any { return dbSnapshot(v) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, id any, code int, msg string) {
	writeJSON(w, jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}
