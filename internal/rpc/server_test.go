package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"odm/internal/download"
	"odm/internal/scheduler"

	"github.com/gorilla/websocket"
)

// helper to start a Server-backed httptest.Server over a live Daemon.
func startServer(t *testing.T, secret string) (*httptest.Server, *Server, func()) {
	t.Helper()
	// Build a Manager (no network needed unless a task actually runs; we only
	// exercise the plumbing in these tests). 1 connection × 8KiB chunks over a
	// few-MiB payload keeps a localhost download slow enough for the 250ms
	// progress throttle to fire a couple of times before completion, which the
	// completion-event test depends on.
	m, err := download.NewManager(download.ExecOptions{
		Dir:         t.TempDir(),
		Connections: 1,
		ChunkSize:   8 * 1024,
		Retry:       1,
		RetryWait:   5 * time.Millisecond,
		Timeout:     10 * time.Second,
		CheckCert:   true,
	}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	bc := NewBroadcaster()
	// Mirror runRPC: a shared Broadcaster feeds both the scheduler's progress
	// callback (onDownloadProgress) and the Server's own emissions
	// (onDownloadStart/Complete), plus the Daemon.OnComplete completion hook.
	progCB := func(live, _ []download.ProgressView) {
		for _, v := range live {
			bc.Broadcast(Event{Method: "onDownloadProgress", Params: dbSnapshot(v)})
		}
	}
	sch := scheduler.NewEmptyScheduler(2, m.NewTask, progCB)
	d := scheduler.NewDaemon(sch, m)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	srv := NewServerWithBroadcaster(d, secret, bc)
	// completion events: route each task's terminal snapshot onto the
	// Broadcaster. Matches what runRPC does, so these tests exercise the real
	// event path rather than a stub.
	d.OnComplete(srv.OnTaskComplete)

	mux := http.NewServeMux()
	srv.Routes(mux)
	hs := httptest.NewServer(mux)
	cleanup := func() {
		hs.Close()
		d.Stop()
		cancel()
	}
	return hs, srv, cleanup
}

// serveRangePayloadRC is a tiny range-supporting payload server for the RPC
// completion-event test (the download package has its own; we keep an
// independent one here so this test doesn't reach across packages).
func serveRangePayloadRC(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaRC(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", itoaRC(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		i := strings.Index(rng, "=")
		rest := rng[i+1:]
		j := strings.Index(rest, "-")
		start, end := atoiRC(rest[:j]), -1
		if j+1 < len(rest) {
			end = atoiRC(rest[j+1:])
		}
		if end < 0 || end >= len(payload) {
			end = len(payload) - 1
		}
		w.Header().Set("Content-Range", "bytes "+itoaRC(start)+"-"+itoaRC(end)+"/"+itoaRC(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

func itoaRC(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func atoiRC(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// waitSubscribers polls until the Broadcaster has at least n registered /ws
// subscribers (addWS registers its pump goroutine async, so a freshly-dialled
// frame would otherwise be dropped before the subscriber channel exists).
func waitSubscribers(t *testing.T, srv *Server, n int) {
	t.Helper()
	bc := srv.Broadcaster()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bc.SubscriberCount() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber never registered on /ws (want %d)", n)
}

func post(t *testing.T, url string, body any) (map[string]any, []any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var rs jsonRPCResponse
	_ = json.Unmarshal(raw, &rs)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m, nil
}

// jsonRPC sends a request and returns the response struct.
func jsonRPC(t *testing.T, url, method string, params []any, id any) jsonRPCResponse {
	t.Helper()
	req := jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params, ID: id}
	b, _ := json.Marshal(req)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r jsonRPCResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal %q: %v", string(raw), err)
	}
	return r
}

func TestServer_AddURIAndTellActive(t *testing.T) {
	hs, _, stop := startServer(t, "")
	defer stop()
	url := hs.URL + "/rpc"

	r := jsonRPC(t, url, "odm.addUri", []any{"http://example.invalid/file.bin", 2}, 1)
	if r.Error != nil {
		t.Fatalf("addUri: %v", r.Error)
	}
	if r.Result == "" {
		t.Fatalf("expected goid string, got %v", r.Result)
	}
	id, _ := r.Result.(string)

	// The task is now in the Scheduler manager's registry, so tellStopped/later
	// tellActive should surface it (it may move between active/waiting since the
	// empty plan had 0 slots — but Enqueue forces admitNext).
	_ = id
	// Poll briefly for it to show up somewhere. example.invalid never resolves,
	// so the probe fails in milliseconds: the task can be gone from active AND
	// waiting before the first poll and land in stopped — that's why tellStopped
	// is part of the loop (the earlier active/waiting-only variant was flaky).
	deadline := time.Now().Add(2 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		r = jsonRPC(t, url, "odm.tellActive", nil, 2)
		if rr, ok := r.Result.([]any); ok && len(rr) > 0 {
			found = true
			break
		}
		r = jsonRPC(t, url, "odm.tellWaiting", nil, 3)
		if rr, ok := r.Result.([]any); ok && len(rr) > 0 {
			found = true
			break
		}
		r = jsonRPC(t, url, "odm.tellStopped", nil, 4)
		if rr, ok := r.Result.([]any); ok && len(rr) > 0 {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("added task never appeared in tellActive/tellWaiting/tellStopped")
	}
}

func TestServer_AuthEnforced(t *testing.T) {
	hs, _, stop := startServer(t, "hunter2")
	defer stop()
	url := hs.URL + "/rpc"

	// No token → auth error.
	r := jsonRPC(t, url, "odm.getVersion", nil, 1)
	if r.Error == nil || r.Error.Code != codeAuthError {
		t.Fatalf("expected auth error, got %+v", r)
	}
	// Good token → OK.
	r = jsonRPC(t, url, "odm.getVersion", []any{"token:hunter2"}, 1)
	if r.Error != nil {
		t.Fatalf("good token should pass: %+v", r.Error)
	}
	// Bad token → auth error.
	r = jsonRPC(t, url, "odm.getVersion", []any{"token:wrong"}, 1)
	if r.Error == nil || r.Error.Code != codeAuthError {
		t.Fatalf("bad token should fail, got %+v", r)
	}
}

func TestServer_GetVersionReturnsFeatures(t *testing.T) {
	hs, _, stop := startServer(t, "")
	defer stop()
	body, _ := post(t, hs.URL+"/rpc", jsonRPCRequest{JSONRPC: "2.0", Method: "odm.getVersion", ID: 1})
	res := body["result"].(map[string]any)
	if res["version"] == nil {
		t.Fatalf("missing version: %v", body)
	}
	feats, _ := res["enabledFeatures"].([]any)
	if len(feats) == 0 {
		t.Fatalf("missing features list")
	}
}

func TestServer_UnknownMethod(t *testing.T) {
	hs, _, stop := startServer(t, "")
	defer stop()
	r := jsonRPC(t, hs.URL+"/rpc", "odm.bogus", nil, 1)
	if r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("expected method-not-found, got %+v", r)
	}
}

func TestServer_GetGlobalStat(t *testing.T) {
	hs, _, stop := startServer(t, "")
	defer stop()
	r := jsonRPC(t, hs.URL+"/rpc", "odm.getGlobalStat", nil, 1)
	if r.Error != nil {
		t.Fatalf("getGlobalStat: %v", r.Error)
	}
	m, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not an object: %v", r.Result)
	}
	if _, ok := m["active"]; !ok {
		t.Fatalf("missing 'active' field: %v", m)
	}
}

// TestServer_ShutdownRespondsThenStops pins the shutdown ordering: the "OK"
// response must be delivered BEFORE the daemon stops (the stop cancels the
// scheduler and, in runRPC, closes the listener — if it raced the write, the
// reply could be truncated). The daemon must then actually wind down.
func TestServer_ShutdownRespondsThenStops(t *testing.T) {
	hs, srv, stop := startServer(t, "")
	defer stop()
	r := jsonRPC(t, hs.URL+"/rpc", "odm.shutdown", nil, 1)
	if r.Error != nil {
		t.Fatalf("shutdown error: %v", r.Error)
	}
	if r.Result != "OK" {
		t.Fatalf("want result OK, got %v", r.Result)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Daemon().Dead() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon did not stop after odm.shutdown")
}

// TestServer_ShutdownRequiresSecret pins the auth gate on odm.shutdown: with a
// secret configured, an unauthenticated shutdown request gets an auth error
// AND leaves the daemon running (it used to shut the daemon down anyway —
// dispatch rejected it, but handleRPC detected the method from the raw request
// and stopped the scheduler regardless).
func TestServer_ShutdownRequiresSecret(t *testing.T) {
	hs, srv, stop := startServer(t, "topsecret")
	defer stop()
	r := jsonRPC(t, hs.URL+"/rpc", "odm.shutdown", nil, 1)
	if r.Error == nil {
		t.Fatalf("unauthenticated odm.shutdown must be rejected, got result %v", r.Result)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.Daemon().Dead() {
			t.Fatal("daemon stopped despite unauthenticated odm.shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServer_ChangeOptionConnectionsRejectsZero pins the boundary guard on
// changeOption {"connections": N}: N < 1 would retire every worker via the
// graceful drain while chunks remain queued, so the task would report
// completed with most bytes missing (and its control file deleted).
func TestServer_ChangeOptionConnectionsRejectsZero(t *testing.T) {
	hs, _, stop := startServer(t, "")
	defer stop()
	url := hs.URL + "/rpc"

	add := jsonRPC(t, url, "odm.addUri", []any{"http://example.invalid/file.bin", 2}, 1)
	if add.Error != nil {
		t.Fatalf("addUri: %v", add.Error)
	}
	gid := add.Result.(string)

	for _, nc := range []int{0, -3} {
		r := jsonRPC(t, url, "odm.changeOption", []any{gid, map[string]any{"connections": float64(nc)}}, 2)
		if r.Error == nil {
			t.Fatalf("connections=%d must be rejected, got result %v", nc, r.Result)
		}
	}
}

func TestBroadcasterEvents(t *testing.T) {
	_, srv, stop := startServer(t, "")
	defer stop()
	// Verify broadcasting to subscribers picks up an emitted event.
	bc := srv.Broadcaster()
	// SubscriberCount should be zero initially.
	if bc.SubscriberCount() != 0 {
		t.Fatalf("expected zero subs, got %d", bc.SubscriberCount())
	}
	// Emitting without subscribers doesn't blow up.
	bc.Broadcast(Event{Method: "onDownloadStart", Params: "x"})
}

// TestServer_WSDialEvent is the spec RPC-over-WebSocket acceptance test: a
// real gorilla/websocket client dials /ws, a separate HTTP POST fires odm.addUri,
// and the subscriber must receive the pushed onDownloadStart frame carrying the
// new task's goid. This exercises the full event path end-to-end: ws upgrade,
// the Broadcaster's addWS pump, dispatch's Broadcast, and the channel→socket
// delivery — not the unit-level Broadcast() call TestBroadcasterEvents checks.
//
// We give the ws read a generous deadline because the subscriber goroutine is
// started in addWS and the dispatch+Broadcast happen on the /rpc goroutine, so
// ordering is async (but the event is emitted synchronously inside addUri's
// dispatch before the RPC response returns).
func TestServer_WSDialEvent(t *testing.T) {
	hs, srv, stop := startServer(t, "")
	defer stop()

	// /ws upgrade URL — httptest.Server.URL is http://127.0.0.1:port; swap scheme.
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws"

	d := &websocket.Dialer{} // no secret configured in this server
	conn, _, err := d.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// Wait for the server's addWS pump goroutine to register the subscriber so
	// the Broadcast below actually reaches a live subscriber — otherwise the
	// non-blocking send to a not-yet-buffered subscriber channel would drop the
	// event before addWS's goroutine starts pumping. Poll the broadcaster count.
	bc := srv.Broadcaster()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bc.SubscriberCount() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if bc.SubscriberCount() < 1 {
		t.Fatalf("subscriber never registered on /ws")
	}

	// Fire addUri over the JSON-RPC endpoint; dispatch emits onDownloadStart.
	rpcURL := hs.URL + "/rpc"
	res := jsonRPC(t, rpcURL, "odm.addUri",
		[]any{"http://example.invalid/ws-event.bin", 1}, 1)
	if res.Error != nil {
		t.Fatalf("addUri: %v", res.Error)
	}
	goid, ok := res.Result.(string)
	if !ok || goid == "" {
		t.Fatalf("addUri result not a goid: %v", res.Result)
	}

	// Read the pushed event frame off the WebSocket. onDownloadStart is the
	// first event after addUri (the engine may later emit more; we only need
	// this one). Give the read a generous deadline so async ordering can't
	// flake under CI load.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		mt, raw, rerr := conn.ReadMessage()
		if rerr != nil {
			t.Fatalf("didn't receive an event frame: %v", rerr)
		}
		if mt != websocket.TextMessage {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("unmarshal event frame %q: %v", string(raw), err)
		}
		if ev.Method != "onDownloadStart" {
			// keep draining; the first event is the one we want.
			continue
		}
		// Verify the event carries the goid we just added.
		params, ok := ev.Params.(map[string]any)
		if !ok {
			t.Fatalf("onDownloadStart params not a map: %T(%v)", ev.Params, ev.Params)
		}
		g, _ := params["goid"].(string)
		if g != goid {
			t.Fatalf("onDownloadStart goid mismatch: want %q got %q", goid, g)
		}
		return
	}
}

// TestServer_WSDialEvent_WithSecret confirms the ?secret=<value> query-auth on
// the /ws upgrade: a missing secret is rejected, the right one upgrades.
func TestServer_WSDialEvent_WithSecret(t *testing.T) {
	hs, _, stop := startServer(t, "topsecret")
	defer stop()

	wsBase := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws"
	d := &websocket.Dialer{}

	// Wrong/no secret → 401, dial returns a bad-status error.
	conn, resp, err := d.Dial(wsBase, nil)
	if err == nil {
		conn.Close()
		t.Fatalf("ws dial with no secret should fail (auth), got a connection")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 on missing secret, got %d", resp.StatusCode)
	}

	// Correct secret in the query upgrades successfully.
	wsSecret := wsBase + "?secret=topsecret"
	conn, _, err = d.Dial(wsSecret, nil)
	if err != nil {
		t.Fatalf("ws dial with correct secret should succeed: %v", err)
	}
	conn.Close()
}

// ensure string helpers used in tests don't optimize away.
var _ = strings.TrimSpace

// TestServer_WSCompletionEvents is the spec RPC-over-WebSocket acceptance test
// for the previously-missing events: it drives a real download through the
// engine (a range-supporting httptest payload) over an RPC addUri, and asserts the
// WebSocket subscriber receives onDownloadStart followed by onDownloadComplete —
// proving the completion→Broadcaster wiring (handleComplete → Daemon.OnComplete →
// Server.OnTaskComplete → Broadcast) fires the terminal event end-to-end.
func TestServer_WSCompletionEvents(t *testing.T) {
	hs, srv, stop := startServer(t, "")
	defer stop()

	// Payload server the engine will actually download from. Large enough that
	// the 250ms progress throttle fires at least once before completion — a tiny
	// payload would finish inside the first throttle tick and emit no progress.
	payload := make([]byte, 4*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	fsrv := serveRangePayloadRC(t, payload)
	defer fsrv.Close()

	// Dial /ws up front.
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws"
	conn, _, err := (&websocket.Dialer{}).Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitSubscribers(t, srv, 1)

	// Add the URL over JSON-RPC; dispatch emits onDownloadStart synchronously.
	rpcURL := hs.URL + "/rpc"
	res := jsonRPC(t, rpcURL, "odm.addUri", []any{fsrv.URL + "/file.bin", 2}, 1)
	if res.Error != nil {
		t.Fatalf("addUri: %v", res.Error)
	}
	goid, _ := res.Result.(string)
	if goid == "" {
		t.Fatalf("addUri returned no goid: %v", res.Result)
	}

	// Drain lifecycle events. We must see onDownloadStart (immediate) and then
	// onDownloadComplete (once the engine finishes the 64 KiB download). The
	// progress throttle may intersperse onDownloadProgress frames.
	got := map[string]bool{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, rerr := conn.ReadMessage()
		if rerr != nil {
			break
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		if ev.Method == "onDownloadStart" || ev.Method == "onDownloadComplete" || ev.Method == "onDownloadError" || ev.Method == "onDownloadProgress" {
			got[ev.Method] = true
		}
		if got["onDownloadStart"] && got["onDownloadProgress"] && (got["onDownloadComplete"] || got["onDownloadError"]) {
			break
		}
	}

	if !got["onDownloadStart"] {
		t.Fatalf("missing onDownloadStart event; seen=%v", got)
	}
	if !got["onDownloadProgress"] {
		t.Fatalf("missing onDownloadProgress event; seen=%v", got)
	}
	if !got["onDownloadComplete"] {
		t.Fatalf("missing onDownloadComplete event (downlaod likely errored); seen=%v", got)
	}
	// A clean download must NOT also emit an error event.
	if got["onDownloadError"] {
		t.Fatalf("unexpected onDownloadError for a clean download; seen=%v", got)
	}

	// tellStatus of the stopped task must read "completed" (proves snapshot used
	// for the event carried the final state too — and exercises tellStopped path).
	pollStop := time.Now().Add(5 * time.Second)
	for time.Now().Before(pollStop) {
		st := jsonRPC(t, rpcURL, "odm.tellStatus", []any{goid}, 2)
		if m, ok := st.Result.(map[string]any); ok {
			if s, _ := m["status"].(string); s == "completed" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stopped task never reported completed via tellStatus")
}

// TestServer_WSErrorEvent asserts the onDownloadError event fires for a
// task that fails (here: an unreachable URL the engine can complete). We point
// the engine at a 500-returning payload server and expect onDownloadError over
// /ws rather than onDownloadComplete.
func TestServer_WSErrorEvent(t *testing.T) {
	hs, srv, stop := startServer(t, "")
	defer stop()

	// A server that 500s every request (HEAD included), exhausting retries into
	// an error state.
	fsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fsrv.Close()

	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws"
	conn, _, err := (&websocket.Dialer{}).Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitSubscribers(t, srv, 1)

	res := jsonRPC(t, hs.URL+"/rpc", "odm.addUri", []any{fsrv.URL + "/file.bin", 1}, 1)
	if res.Error != nil {
		t.Fatalf("addUri: %v", res.Error)
	}

	got := map[string]bool{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, rerr := conn.ReadMessage()
		if rerr != nil {
			break
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		got[ev.Method] = true
		if got["onDownloadStart"] && got["onDownloadError"] {
			break
		}
	}
	if !got["onDownloadStart"] {
		t.Fatalf("missing onDownloadStart; seen=%v", got)
	}
	if !got["onDownloadError"] {
		t.Fatalf("missing onDownloadError for a failing task; seen=%v", got)
	}
}
