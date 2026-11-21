//! Rust port of ODM's `internal/rpc/types.go` — JSON-RPC 2.0 envelopes and the
//! WebSocket notification shape.

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// JSON-RPC 2.0 request envelope. Missing `params`/`id` default to empty/null,
/// mirroring Go's `omitempty`-tagged fields decoded from absent JSON keys.
#[derive(Debug, Clone, Deserialize)]
pub struct JsonRpcRequest {
    #[serde(default)]
    pub jsonrpc: String,
    #[serde(default)]
    pub method: String,
    #[serde(default)]
    pub params: Vec<Value>,
    #[serde(default)]
    pub id: Value,
}

/// JSON-RPC 2.0 response (result XOR error). `result` is skipped when absent so
/// error responses never carry a `result: null` field (Go's `omitempty`).
#[derive(Debug, Clone, Serialize)]
pub struct JsonRpcResponse {
    pub jsonrpc: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<RpcError>,
    pub id: Value,
}

impl JsonRpcResponse {
    pub fn ok(id: Value, result: Value) -> JsonRpcResponse {
        JsonRpcResponse { jsonrpc: "2.0", result: Some(result), error: None, id }
    }
    pub fn err(id: Value, code: i32, message: impl Into<String>) -> JsonRpcResponse {
        JsonRpcResponse {
            jsonrpc: "2.0",
            result: None,
            error: Some(RpcError { code, message: message.into() }),
            id,
        }
    }
}

/// JSON-RPC 2.0 error object.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RpcError {
    pub code: i32,
    pub message: String,
}

/// Standard JSON-RPC 2.0 error codes plus the app-level auth error.
pub const CODE_PARSE_ERROR: i32 = -32700;
pub const CODE_METHOD_NOT_FOUND: i32 = -32601;
pub const CODE_INVALID_PARAMS: i32 = -32602;
pub const CODE_INTERNAL_ERROR: i32 = -32603;
pub const CODE_AUTH_ERROR: i32 = 1;

/// A WebSocket notification pushed to subscribers.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Event {
    pub method: String,
    pub params: Value,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_defaults_missing_fields() {
        let r: JsonRpcRequest = serde_json::from_str(r#"{"method":"odm.noop"}"#).unwrap();
        assert_eq!(r.method, "odm.noop");
        assert!(r.params.is_empty());
        assert_eq!(r.id, Value::Null);
    }

    #[test]
    fn response_serialization_omits_absent_result() {
        let ok = serde_json::to_value(JsonRpcResponse::ok(Value::from(1), Value::from("OK"))).unwrap();
        assert_eq!(ok["result"], Value::from("OK"));
        assert!(ok.get("error").is_none());

        let err = serde_json::to_value(JsonRpcResponse::err(Value::from(2), CODE_AUTH_ERROR, "bad")).unwrap();
        assert!(err.get("result").is_none());
        assert_eq!(err["error"]["code"], Value::from(CODE_AUTH_ERROR));
        assert_eq!(err["id"], Value::from(2));
    }
}
