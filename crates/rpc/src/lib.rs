//! Rust port of ODM's `internal/rpc` package: JSON-RPC 2.0 over HTTP POST at
//! `/rpc`, WebSocket event fan-out at `/ws`, and aria2-style `token:<secret>`
//! authentication. The server delegates to `scheduler::Daemon`, so methods map
//! onto the same code path the one-shot CLI uses.

pub mod server;
pub mod throttler;
pub mod types;
pub mod ws;

pub use server::{snapshot_params, Server};
pub use throttler::ProgressThrottler;
pub use types::{Event, JsonRpcRequest, JsonRpcResponse, RpcError, CODE_AUTH_ERROR, CODE_INVALID_PARAMS, CODE_INTERNAL_ERROR, CODE_METHOD_NOT_FOUND, CODE_PARSE_ERROR};
pub use ws::Broadcaster;
