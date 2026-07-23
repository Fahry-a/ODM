// Package rpc implements ODM's JSON-RPC 2.0 + WebSocket control surface (PRD
// §10): JSON-RPC 2.0 over HTTP POST at /rpc, WebSocket event fan-out at /ws,
// and aria2-style `token:<secret>` authentication when --rpc-secret is set.
//
// The server delegates to a scheduler.Daemon (which owns the live Scheduler + a
// download.Manager), so methods like addUri/tellStatus/pause map directly onto
// the same code path the one-shot CLI uses.
package rpc

// jsonRPCRequest is a JSON-RPC 2.0 request envelope.
type jsonRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  []any `json:"params,omitempty"`
	ID      any   `json:"id,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response (result XOR error).
type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  any `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
	ID      any `json:"id"`
}

// rpcError follows the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Standard JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeAuthError      = 1 // app-level: bad/missing secret
)

// Event is a WebSocket notification pushed to subscribers (PRD §10.3).
type Event struct {
	Method string      `json:"method"`
	Params any `json:"params"`
}
