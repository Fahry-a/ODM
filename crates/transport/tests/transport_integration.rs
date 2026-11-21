//! Port of internal/transport/transport_test.go (Go): probe/get_range tests
//! against in-process mini HTTP servers and an in-process SOCKS5 relay.

use std::collections::HashMap;
use std::io::{self, Read, Write};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, Shutdown, TcpListener, TcpStream};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use transport::{new_client, ClientConfig};

// --- helpers: minimal HTTP/1.1 servers ---

fn read_headers(s: &mut TcpStream) -> Vec<u8> {
    let mut buf = Vec::new();
    let mut tmp = [0u8; 4096];
    loop {
        match s.read(&mut tmp) {
            Ok(0) => return buf,
            Ok(n) => {
                buf.extend_from_slice(&tmp[..n]);
                if buf.windows(4).any(|w| w == b"\r\n\r\n")
                    || buf.windows(2).any(|w| w == b"\n\n")
                {
                    return buf;
                }
            }
            Err(_) => return buf,
        }
    }
}

fn method_of(head: &[u8]) -> String {
    String::from_utf8_lossy(head).split_whitespace().nth(0).unwrap_or("").to_string()
}

fn header_of(head: &[u8], key: &str) -> String {
    let head = String::from_utf8_lossy(head).to_string();
    for line in head.split("\r\n") {
        if let Some((k, v)) = line.split_once(':') {
            if k.trim().eq_ignore_ascii_case(key) {
                return v.trim().to_string();
            }
        }
    }
    String::new()
}

/// `serve_range` is a tiny handler supporting "Range: bytes=a-b" (206) and a
/// plain 200 for non-range requests (mirrors the Go test's serveRange).
fn serve_range(payload: &[u8], block_head: bool) -> String {
    let ln = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/", ln.local_addr().unwrap());
    let payload = payload.to_vec();
    std::thread::spawn(move || {
        for stream in ln.incoming() {
            let mut s = match stream {
                Ok(s) => s,
                Err(_) => break,
            };
            let payload = payload.clone();
            std::thread::spawn(move || {
                let head = read_headers(&mut s);
                if block_head && method_of(&head) == "HEAD" {
                    let _ = s.write_all(b"HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\nContent-Length: 0\r\n\r\n");
                    return;
                }
                let rng = header_of(&head, "range");
                if !rng.is_empty() {
                    if let Some((start, end)) = parse_client_range(&rng, payload.len()) {
                        let resp = format!(
                            "HTTP/1.1 206 Partial Content\r\nConnection: close\r\nAccept-Ranges: bytes\r\nContent-Range: bytes {}-{}/{}\r\nContent-Length: {}\r\n\r\n",
                            start,
                            end,
                            payload.len(),
                            end - start + 1
                        );
                        let _ = s.write_all(resp.as_bytes());
                        let _ = s.write_all(&payload[start..=end]);
                        return;
                    }
                }
                let resp = format!(
                    "HTTP/1.1 200 OK\r\nConnection: close\r\nAccept-Ranges: bytes\r\nContent-Length: {}\r\n\r\n",
                    payload.len()
                );
                let _ = s.write_all(resp.as_bytes());
                let _ = s.write_all(&payload);
            });
        }
    });
    url
}

/// Server that ignores Range and always serves 200 with full body.
fn serve_no_range(payload: &[u8]) -> String {
    let ln = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/", ln.local_addr().unwrap());
    let payload = payload.to_vec();
    std::thread::spawn(move || {
        for stream in ln.incoming() {
            let mut s = match stream {
                Ok(s) => s,
                Err(_) => break,
            };
            let payload = payload.clone();
            std::thread::spawn(move || {
                let _head = read_headers(&mut s);
                let resp = format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n", payload.len());
                let _ = s.write_all(resp.as_bytes());
                let _ = s.write_all(&payload);
            });
        }
    });
    url
}

/// Server that answers HEAD with 200 and GET with 200 + "hello" (no
/// Content-Length).
fn serve_sizeless() -> String {
    let ln = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/", ln.local_addr().unwrap());
    std::thread::spawn(move || {
        for stream in ln.incoming() {
            let mut s = match stream {
                Ok(s) => s,
                Err(_) => break,
            };
            std::thread::spawn(move || {
                let head = read_headers(&mut s);
                if method_of(&head) == "HEAD" {
                    let _ = s.write_all(b"HTTP/1.1 200 OK\r\n\r\n");
                    return;
                }
                let _ = s.write_all(b"HTTP/1.1 200 OK\r\n\r\nhello");
                let _ = s.shutdown(Shutdown::Write);
            });
        }
    });
    url
}

/// Redirect-loop server: every request gets a 302 to "/".
fn serve_redirect_loop() -> String {
    let ln = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/", ln.local_addr().unwrap());
    std::thread::spawn(move || {
        for stream in ln.incoming() {
            let mut s = match stream {
                Ok(s) => s,
                Err(_) => break,
            };
            std::thread::spawn(move || {
                let _head = read_headers(&mut s);
                let _ = s.write_all(b"HTTP/1.1 302 Found\r\nLocation: /\r\nContent-Length: 0\r\n\r\n");
            });
        }
    });
    url
}

fn parse_client_range(h: &str, total: usize) -> Option<(usize, usize)> {
    // "bytes=4-7"
    let (_, rng) = h.split_once('=')?;
    let (sstr, estr) = rng.split_once('-')?;
    let mut e = atoi(estr);
    let s = atoi(sstr);
    if e < 0 || e >= total as i64 {
        e = total as i64 - 1;
    }
    Some((s as usize, e as usize))
}

fn atoi(s: &str) -> i64 {
    let mut n = 0i64;
    for c in s.chars() {
        if c < '0' || c > '9' {
            break;
        }
        n = n * 10 + (c as i64 - '0' as i64);
    }
    n
}

// --- tests ---

#[tokio::test]
async fn test_probe_head_accepts_ranges() {
    let url = serve_range(&vec![0u8; 1000], false);
    let c = new_client(ClientConfig { max_redirect: 5, check_certificate: true, ..Default::default() }).unwrap();
    let pr = c.probe(&url).await.unwrap();
    assert!(pr.supports_range, "want range support, got {pr:?}");
    assert_eq!(pr.total_size, 1000);
    assert!(!pr.single_stream, "should not be single-stream");
}

#[tokio::test]
async fn test_probe_head_blocked_falls_back_to_range_get() {
    let url = serve_range(&vec![0u8; 500], true /* block HEAD */);
    let c = new_client(ClientConfig { max_redirect: 5, ..Default::default() }).unwrap();
    let pr = c.probe(&url).await.unwrap();
    assert!(pr.supports_range, "range probe fallback must succeed, got {pr:?}");
    assert_eq!(pr.total_size, 500);
}

#[tokio::test]
async fn test_probe_no_range_support_single_stream() {
    let url = serve_no_range(&vec![0u8; 256]);
    let c = new_client(ClientConfig { max_redirect: 5, ..Default::default() }).unwrap();
    let pr = c.probe(&url).await.unwrap();
    assert!(!pr.supports_range, "must report no range support");
    assert!(pr.single_stream, "want single-stream, got {pr:?}");
    assert_eq!(pr.total_size, 256);
}

#[tokio::test]
async fn test_probe_sizeless_stream() {
    let url = serve_sizeless();
    let c = new_client(ClientConfig { max_redirect: 5, ..Default::default() }).unwrap();
    let pr = c.probe(&url).await.unwrap();
    assert_eq!(pr.total_size, -1, "want sizeless (-1)");
    assert!(pr.single_stream, "sizeless must be single-stream");
}

#[tokio::test]
async fn test_get_range_partial_content() {
    let url = serve_range(b"0123456789ABCDEF", false);
    let c = new_client(ClientConfig { max_redirect: 5, ..Default::default() }).unwrap();
    let resp = c.get_range(&url, 4, 7, "").await.unwrap();
    assert_eq!(resp.status().as_u16(), 206);
    let got = resp.bytes().await.unwrap();
    assert_eq!(&got[..], b"4567");
}

#[tokio::test]
async fn test_redirect_limit() {
    let url = serve_redirect_loop();
    let c = new_client(ClientConfig { max_redirect: 2, ..Default::default() }).unwrap();
    assert!(c.probe(&url).await.is_err());
}

// --- SOCKS5 proxy tests ---

const SOCKS_ATYP_IPV4: u8 = 0x01;
const SOCKS_ATYP_IPV6: u8 = 0x04;
const SOCKS_ATYP_FQDN: u8 = 0x03;

/// One CONNECT request the proxy received.
#[derive(Clone, Debug)]
struct SocksReq {
    atyp: u8,             // address type as sent by the client
    host: String,         // target host as sent by the client (IP literal or FQDN)
    #[allow(dead_code)]
    port: u16, // target port as sent by the client
}

/// Minimal in-process SOCKS5 server (RFC 1928): greeting + method negotiation,
/// a CONNECT request, then a plain bidirectional TCP relay. Records every
/// CONNECT. map_host lets the "proxy" resolve hostnames the client cannot
/// (e.g. fake-proxy-only.invalid → 127.0.0.1).
struct TestSocksServer {
    ln: TcpListener,
    map_host: HashMap<String, String>,
    reqs: Arc<Mutex<Vec<SocksReq>>>,
}

impl TestSocksServer {
    fn new(map_host: HashMap<String, String>) -> Arc<TestSocksServer> {
        let ln = TcpListener::bind("127.0.0.1:0").unwrap();
        let s = Arc::new(TestSocksServer { ln, map_host, reqs: Arc::new(Mutex::new(Vec::new())) });
        let s2 = Arc::clone(&s);
        std::thread::spawn(move || s2.accept_loop());
        s
    }

    fn addr(&self) -> String {
        self.ln.local_addr().unwrap().to_string()
    }

    fn requests(&self) -> Vec<SocksReq> {
        self.reqs.lock().unwrap().clone()
    }

    fn accept_loop(self: Arc<Self>) {
        for stream in self.ln.incoming() {
            if let Ok(c) = stream {
                let s = Arc::clone(&self);
                std::thread::spawn(move || s.handle_conn(c));
            }
        }
    }

    fn handle_conn(self: Arc<Self>, mut c: TcpStream) {
        let _ = c.set_read_timeout(Some(Duration::from_secs(10)));

        // Greeting: VER(5) NMETHODS METHODS...
        let mut hdr = [0u8; 2];
        if c.read_exact(&mut hdr).is_err() || hdr[0] != 5 {
            return;
        }
        let mut methods = vec![0u8; hdr[1] as usize];
        if c.read_exact(&mut methods).is_err() {
            return;
        }
        let _ = c.write_all(&[5, 0]);

        // CONNECT request: VER(5) CMD(1) RSV(0) ATYP ADDR PORT.
        let mut req = [0u8; 4];
        if c.read_exact(&mut req).is_err() || req[0] != 5 || req[1] != 1 || req[2] != 0 {
            return;
        }
        let host = match read_socks_addr(&mut c, req[3]) {
            Some(h) => h,
            None => return,
        };
        let mut portb = [0u8; 2];
        if c.read_exact(&mut portb).is_err() {
            return;
        }
        let port = u16::from_be_bytes(portb);
        self.reqs.lock().unwrap().push(SocksReq { atyp: req[3], host: host.clone(), port });

        // Resolve the target: an explicit mapping wins; loopback goes to
        // 127.0.0.1 (the origin listens on IPv4 loopback only; the client may
        // legitimately resolve "localhost" to ::1).
        let resolved = self
            .map_host
            .get(&host)
            .cloned()
            .or_else(|| {
                if let Ok(ip) = host.parse::<IpAddr>() {
                    if ip.is_loopback() {
                        return Some("127.0.0.1".to_string());
                    }
                }
                None
            })
            .unwrap_or(host);
        let mut target = match TcpStream::connect((resolved.as_str(), port)) {
            Ok(t) => t,
            Err(_) => {
                let _ = c.write_all(&[5, 5, 0, 1, 0, 0, 0, 0, 0, 0]);
                return;
            }
        };
        let _ = c.write_all(&[5, 0, 0, 1, 0, 0, 0, 0, 0, 0]);
        let _ = c.set_read_timeout(None);

        // Plain TCP relay in both directions; half-close so both sides see
        // EOF.
        let mut tc = target.try_clone().unwrap();
        let mut cc = c.try_clone().unwrap();
        std::thread::spawn(move || {
            let _ = io::copy(&mut tc, &mut cc);
            let _ = cc.shutdown(Shutdown::Write);
        });
        let _ = io::copy(&mut c, &mut target);
        let _ = target.shutdown(Shutdown::Write);
    }
}

fn read_socks_addr(c: &mut TcpStream, atyp: u8) -> Option<String> {
    match atyp {
        SOCKS_ATYP_IPV4 => {
            let mut b = [0u8; 4];
            c.read_exact(&mut b).ok().map(|_| Ipv4Addr::new(b[0], b[1], b[2], b[3]).to_string())
        }
        SOCKS_ATYP_IPV6 => {
            let mut b = [0u8; 16];
            c.read_exact(&mut b).ok().map(|_| Ipv6Addr::from(b).to_string())
        }
        SOCKS_ATYP_FQDN => {
            let mut l = [0u8; 1];
            c.read_exact(&mut l).ok()?;
            let mut b = vec![0u8; l[0] as usize];
            c.read_exact(&mut b).ok()?;
            Some(String::from_utf8_lossy(&b).into_owned())
        }
        _ => None,
    }
}

/// socksOrigin serves payload with Range support and sets Connection: close
/// so the relay terminates deterministically once the response is delivered.
fn socks_origin(payload: &[u8]) -> String {
    let ln = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/", ln.local_addr().unwrap());
    let payload = payload.to_vec();
    std::thread::spawn(move || {
        for stream in ln.incoming() {
            let mut s = match stream {
                Ok(s) => s,
                Err(_) => break,
            };
            let payload = payload.clone();
            std::thread::spawn(move || {
                let head = read_headers(&mut s);
                let rng = header_of(&head, "range");
                let mut resp = String::from("HTTP/1.1 200 OK\r\nConnection: close\r\nAccept-Ranges: bytes\r\n");
                if rng.is_empty() {
                    resp.push_str(&format!("Content-Length: {}\r\n\r\n", payload.len()));
                    let _ = s.write_all(resp.as_bytes());
                    let _ = s.write_all(&payload);
                    return;
                }
                let (start, end) = match parse_client_range(&rng, payload.len()) {
                    Some((s_, mut e)) => {
                        if rng.ends_with('-') {
                            e = payload.len() - 1;
                        }
                        (s_, e)
                    }
                    None => {
                        let _ = s.write_all(b"HTTP/1.1 416 Range Not Satisfiable\r\nContent-Length: 0\r\n\r\n");
                        return;
                    }
                };
                resp = resp.replacen("HTTP/1.1 200 OK", "HTTP/1.1 206 Partial Content", 1);
                resp.push_str(&format!(
                    "Content-Range: bytes {}-{}/{}\r\nContent-Length: {}\r\n\r\n",
                    start,
                    end,
                    payload.len(),
                    end - start + 1
                ));
                let _ = s.write_all(resp.as_bytes());
                let _ = s.write_all(&payload[start..=end]);
            });
        }
    });
    url
}

#[tokio::test]
async fn test_socks5_proxy_download() {
    let payload = b"0123456789abcdef-socks5-through-the-proxy".to_vec();
    let origin = socks_origin(&payload);
    let px = TestSocksServer::new(HashMap::new());
    let c = new_client(ClientConfig {
        proxy: format!("socks5://{}", px.addr()),
        max_redirect: 5,
        check_certificate: true,
        timeout: Duration::from_secs(10),
        ..Default::default()
    })
    .unwrap();

    // Target the origin by hostname: with socks5 the client must resolve it
    // locally and send an IP address to the proxy.
    let target = origin.replacen("127.0.0.1", "localhost", 1);
    let resp = c.get_range(&target, 0, -1, "").await.unwrap();
    assert_eq!(resp.status().as_u16(), 206);
    let got = resp.bytes().await.unwrap();
    assert_eq!(&got[..], payload.as_slice());

    let reqs = px.requests();
    assert!(!reqs.is_empty(), "proxy saw no CONNECT requests");
    for r in reqs {
        assert!(
            r.atyp == SOCKS_ATYP_IPV4 || r.atyp == SOCKS_ATYP_IPV6,
            "socks5 must send an IP to the proxy, got atyp={} host={:?}",
            r.atyp,
            r.host
        );
    }
}

#[tokio::test]
async fn test_socks5h_proxy_side_dns() {
    let payload = b"socks5h-resolves-at-the-proxy".to_vec();
    let origin = socks_origin(&payload);
    let port = origin
        .rsplit(':')
        .next()
        .unwrap()
        .trim_end_matches('/')
        .to_string();

    // fake-proxy-only.invalid is unresolvable on the client side; only the
    // proxy knows how to reach it. With socks5h the client must send the
    // hostname and let the proxy resolve it.
    let fake_host = "fake-proxy-only.invalid";
    let mut map = HashMap::new();
    map.insert(fake_host.to_string(), "127.0.0.1".to_string());
    let px = TestSocksServer::new(map);
    let c = new_client(ClientConfig {
        proxy: format!("socks5h://{}", px.addr()),
        max_redirect: 5,
        timeout: Duration::from_secs(10),
        ..Default::default()
    })
    .unwrap();

    let target = format!("http://{fake_host}:{port}/");
    let resp = c.get_range(&target, 0, payload.len() as i64 - 1, "").await.unwrap();
    assert_eq!(resp.status().as_u16(), 206);
    let got = resp.bytes().await.unwrap();
    assert_eq!(&got[..], payload.as_slice());

    let found = px.requests().iter().any(|r| r.atyp == SOCKS_ATYP_FQDN && r.host == fake_host);
    assert!(found, "proxy never saw a CONNECT for {fake_host:?} (proxy-side DNS not used)");
}

#[test]
fn test_http_proxy_still_wired_via_transport_proxy() {
    // Go: tr.Proxy != nil with http user:pass proxy. reqwest: construction
    // succeeds; asserting the internal transport pointer is not possible on
    // reqwest's API, so we pin "valid construction + URL accepted".
    let c = new_client(ClientConfig {
        proxy: "http://user:pass@127.0.0.1:3128".into(),
        max_redirect: 5,
        ..Default::default()
    });
    assert!(c.is_ok(), "http proxy must be accepted");
}

#[test]
fn test_socks5_proxy_invalid_url() {
    for u in [
        "socks5://",                // missing host
        "socks5://:1080",           // empty host
        "socks5://127.0.0.1:0",     // port out of range
        "socks5://127.0.0.1:99999", // port out of range
        "socks5h://[::1",           // url parse rejects
        "socks5://host:notaport",   // url parse rejects
        "ftp://example.com",        // unsupported scheme
    ] {
        assert!(
            new_client(ClientConfig { proxy: u.to_string(), ..Default::default() }).is_err(),
            "NewClient(Proxy={u:?}): expected an error, got nil"
        );
    }
}
