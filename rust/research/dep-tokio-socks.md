# tokio-socks port notes

Source: docs.rs/tokio-socks/0.5.3 · github.com/sticnarf/tokio-socks (Cargo.toml, src/tcp/socks5.rs, src/error.rs, src/lib.rs).

- **Version/defaults**: latest 0.5.3 (docs.rs, 11 Oct 2026). Deps: futures-util 0.3, either 1, thiserror 1, tokio 1.0 (optional). Features: `default` (enables `tokio`), `tokio`, `futures-io`, `tor`. **There is no `proxy-commands` feature in 0.5.3.** Commands: CONNECT 0x01, BIND 0x02, ASSOCIATE declared but unimplemented; with `tor`: TorResolve 0xF0 / TorResolvePtr 0xF1.
- **No `SocksConfig` type exists** in this crate, and no `proxied_dial` — that API is not here. Public API: `Socks5Stream<S>`, `Socks5Listener<S>`, `SocksConnector`.
- **Auth API**: `Authentication` is private (src/lib.rs). Public password path: `connect_with_password(proxy, target, username, password)` and `connect_with_password_and_socket(socket, target, username, password)`; non-password: `connect` / `connect_with_socket`. There is no `with_password` builder method. Username/password lengths must be 1..=255 (`validate_auth` → `Error::InvalidAuthValues`).
- **Errors** (`Error`, thiserror): `Io`, `ParseError`, `InvalidTargetAddress(&'static str)`, `ProxyServerUnreachable`, `InvalidResponseVersion`, `NoAcceptableAuthMethods`, `UnknownAuthMethod`, `GeneralSocksServerFailure`, `ConnectionNotAllowedByRuleset`, `NetworkUnreachable`, `HostUnreachable`, `ConnectionRefused`, `TtlExpired`, `CommandNotSupported`, `AddressTypeNotSupported`, `UnknownError`, `InvalidReservedByte`, `UnknownAddressType`, `InvalidAuthValues(&'static str)`, `PasswordAuthFailure(u8)`, `AuthorizationRequired`, `IdentdAuthFailure`, `InvalidUserIdAuthFailure`.
- **Wire**: greeting `05 01 00` (no auth) or `05 02 00 02`; password `01 len user len pass`; request `05 cmd 00 atyp addr:port`; reply `05 status 00 atyp addr:port`. Domain target → atyp 0x03: the proxy resolves DNS (proxy-side, i.e. socks5h semantics); there is no client-side DNS mode.
- **Go→Rust sketch** for `proxy.SOCKS5("tcp", "h:p", auth, dialer)` (golang.net/x/net/proxy/proxy/socks5.go; ODM: `proxy.FromURL` in `newSocks5Dialer`, internal/transport/transport.go):

```rust
use tokio_socks::Socks5Stream;

// proxy hop first arg ("h:p" = proxy addr); target second arg (domain → ATYP 0x03, proxy resolves)
let stream = Socks5Stream::connect(("proxy.example", 1080u16), ("target.example", 443u16)).await?;
let stream = Socks5Stream::connect_with_password(("proxy.example", 1080u16), ("target.example", 443u16), "user", "pass").await?;
```

  Go: `&proxy.Auth{User, Password}` → `connect_with_password`; Go returns a dialer whose `Dial(network, target)` does the handshake, so the Rust call passes target inline. Go's `dialer` (underlying TCP to the proxy) has no Rust equivalent param — tokio-socks dials the proxy with plain `TcpStream::connect`, no timeout. `Socks5Stream` derefs to `tokio::net::TcpStream`; `into_inner()` unwraps.
