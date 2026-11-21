# tokio-tungstenite research — porting ODM RPC daemon to WebSocket

Library: `/snapview/tokio-tungstenite` (tokio-tungstenite, High reputation, v0.30.0 available; default features `["connect", "handshake"]`). It wraps the full RFC 6455 implementation of the `tungstenite` crate in a Tokio-native async API.

## Server setup — `WebSocketStream<TcpStream>`

Verified server-side entry points (both return `Result<WebSocketStream<S>, WsError>`):

- `tokio::net::TcpListener::bind("127.0.0.1:6900").await?` → `listener.accept()` → `tokio::net::TcpStream`, then:
- `accept_async(stream).await` — plain server handshake; the HTTP response is not exposed on the server side.
- `accept_hdr_async(stream, callback).await` — same, plus a `Callback` (`FnOnce(&Request, Response) -> StdResult<Response, ErrorResponse>`, re-exported at `tungstenite::handshake::server::{Request, Response}`). The callback can mutate response headers (e.g. `X-Server`) or reject with a non-success `Err(response)`; the handshake then fails and the client gets the error response.

`WebSocketStream<S>` (the return type, generic over the IO — `TcpStream` here) is used directly as a `Stream + Sink`; there is no separate builder step needed for the basic accept flow. Error surface: `WsError` — `Protocol(...)` for malformed handshakes (missing `Connection: upgrade` / `Upgrade: websocket` / `Sec-WebSocket-Version: 13` / key headers, junk after the request line, `CustomResponseSuccessful` on server side), `Io(...)` for IO errors; `HandshakeError::Failure` unwraps to the inner error.

## Message types

`tokio_tungstenite::tungstenite::Message` (re-export of `tungstenite::protocol::Message`, v0.30.0):

```rust
pub enum Message {
    Text(Utf8Bytes),
    Binary(Bytes),
    Ping(Bytes),          // payload < 125 bytes
    Pong(Bytes),          // payload < 125 bytes
    Close(Option<CloseFrame>),
    Frame(Frame),         // write-only
}
```

- Constructors: `Message::text(S: Into<Utf8Bytes>)`, `Message::binary(B: Into<Bytes>)`; `From` impls for `String`, `&str`, `Bytes`, `Vec<u8>`.
- Predicates/accessors: `is_text/is_binary/is_ping/is_pong/is_close`, `len()`, `is_empty()`, `to_text() -> Result<&str>`, `into_text() -> Result<Utf8Bytes>`.
- Writing a text frame: `writer.send(Message::text("{\"jsonrpc\":\"2.0\",...}")).await?` via `SinkExt::send`.
- Close: `WebSocketStream::close(Option<CloseFrame>)`; `Message::Close(...)` is the on-the-wire close. Replies to peer pings are automatic.
- Note: `WebSocketBuilder` exists in the ecosystem but the two verified entry points above are sufficient for the daemon; no extra builder step is needed.

## Stream, backpressure

- Reading: `futures_util::StreamExt::next` on the `WebSocketStream` → `Item = Result<Message, WsError>`; returns `None` after the peer closes; break on `msg.is_close()`.
- Typical split pattern: `let (mut writer, reader) = ws_stream.split();` (`StreamExt::split`).
- Writing: `writer.send(Message::text(payload)).await?`, then `writer.flush().await?` to drain.
- Backpressure: `WebSocketStream` implements `Sink<Message>`; `send` resolves only when the write path accepts the frame, so no unbounded internal queue is required to reason about — backpressure falls out of the Sink/stream contract rather than a crate-specific limiter.
- JSON-RPC 2.0 is the payload protocol, defined by you on top of Text frames: the crate has no built-in JSON-RPC layer, so framing/request/response correlation is hand-rolled on frames (mirrors Go: gorilla/websocket + your own RPC semantics).

## Feature flags (Cargo.toml)

- Defaults: `["connect", "handshake"]` — enough for the server (`handshake`) and client (`connect`); `connect` also pulls `tokio/net`, `stream`, `handshake`.
- TLS — only needed if going `wss://`; ODM targets `ws://` (`127.0.0.1:6900`), so none needed by default:
  - `rustls-tls-native-roots` (rustls + OS roots), `rustls-tls-webpki-roots` (rustls + bundled webpki roots — e.g. `features = ["rustls-tls-webpki-roots"]`), `native-tls`, `native-tls-vendored` (vendored OpenSSL). None enabled by default.
  - Caveat: with rustls features and tokio-tungstenite ≥ 0.23 there is a known panic issue (snapview/tokio-tungstenite#336) to be aware of.
- `url` — adds `url::Url` impls for `IntoClientRequest` (client-side).
- `stream` — exposes `MaybeTlsStream` (pulled in by `connect`/TLS features anyway).

## Reconnect behavior

Not provided by the crate — it only covers the handshake and message IO. A reconnect loop (TcpStream::connect + accept_async on the server side / connect_async + client, exponential backoff, re-handshake, replay of unacknowledged requests) must be built around the accept/connect calls in the daemon.
