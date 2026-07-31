package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveRange is a tiny handler supporting "Range: bytes=a-b" (206) and a plain
// 200 for non-range requests. total bytes = len(payload).
func serveRange(t *testing.T, payload []byte, blockHEAD bool) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		if blockHEAD && r.Method == http.MethodHead {
			http.Error(w, "no head", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(len(payload)))
		rng := r.Header.Get("Range")
		if rng == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		start, end, ok := parseClientRange(rng, len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoa(int(start))+"-"+itoa(int(end))+"/"+itoa(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

func TestProbe_HEADAcceptsRanges(t *testing.T) {
	srv := serveRange(t, make([]byte, 1000), false)
	defer srv.Close()
	c, err := NewClient(ClientConfig{MaxRedirect: 5, CheckCertificate: true})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := c.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !pr.SupportsRange || pr.TotalSize != 1000 {
		t.Fatalf("want range+size1000, got %+v", pr)
	}
	if pr.SingleStream {
		t.Fatalf("should not be single-stream")
	}
}

func TestProbe_HEADBlockedFallsBackToRangeGet(t *testing.T) {
	srv := serveRange(t, make([]byte, 500), true /* block HEAD */)
	defer srv.Close()
	c, _ := NewClient(ClientConfig{MaxRedirect: 5})
	pr, err := c.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !pr.SupportsRange || pr.TotalSize != 500 {
		t.Fatalf("range probe fallback must succeed, got %+v", pr)
	}
}

func TestProbe_NoRangeSupportSingleStream(t *testing.T) {
	// Server that ignores Range and always serves 200 with full body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "256")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 256))
	}))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{MaxRedirect: 5})
	pr, err := c.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if pr.SupportsRange {
		t.Fatalf("must report no range support, got %+v", pr)
	}
	if !pr.SingleStream || pr.TotalSize != 256 {
		t.Fatalf("want single-stream with size 256, got %+v", pr)
	}
}

func TestProbe_SizelessStream(t *testing.T) {
	// Chunked streaming, no Content-Length, HEAD returns no size, GET 200 no range.
	// We flush before WriteHeader-equivalent writes so Go's server does not
	// buffer-and-inject Content-Length.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		f, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		if f != nil {
			f.Flush()
		}
		_, _ = w.Write([]byte("hello"))
		if f != nil {
			f.Flush()
		}
	}))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{MaxRedirect: 5})
	pr, err := c.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if pr.TotalSize != -1 {
		t.Fatalf("want sizeless (-1), got %d", pr.TotalSize)
	}
	if !pr.SingleStream {
		t.Fatalf("sizeless must be single-stream")
	}
}

func TestGetRange_PartialContent(t *testing.T) {
	payload := []byte("0123456789ABCDEF")
	srv := serveRange(t, payload, false)
	defer srv.Close()
	c, _ := NewClient(ClientConfig{MaxRedirect: 5})
	rr, err := c.GetRange(context.Background(), srv.URL, 4, 7)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rr.Resp.Body.Close()
	if !rr.SupportsRange {
		t.Fatalf("expected 206 partial content")
	}
	got, _ := io.ReadAll(rr.Resp.Body)
	if string(got) != "4567" {
		t.Fatalf("want '4567', got %q", got)
	}
}

func TestRedirectLimit(t *testing.T) {
	// Chain that always 302s to itself.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{MaxRedirect: 2})
	_, err := c.Probe(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("want error for redirect loop")
	}
}

// --- test helpers ---

func parseClientRange(h string, total int) (start, end int64, ok bool) {
	// "bytes=4-7"
	_, rng, found := cut(h, "=")
	if !found {
		return 0, 0, false
	}
	sstr, estr, found := cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	s := int64(atoi(sstr))
	e := int64(atoi(estr))
	if e < 0 || e >= int64(total) {
		e = int64(total) - 1
	}
	return s, e, true
}

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// --- SOCKS5 test server + proxy tests ---
//
// The proxy tests use a minimal in-process SOCKS5 server (RFC 1928): greeting
// + method negotiation, a CONNECT request, then a plain bidirectional TCP
// relay. No external dependencies, no authentication. The server records every
// CONNECT it receives so tests can assert the address type the client sent —
// which pins the socks5 vs socks5h DNS semantics.

const (
	socksAtypIPv4 = 0x01
	socksAtypIPv6 = 0x04
	socksAtypFQDN = 0x03
)

// socksReq is one CONNECT request the test proxy received.
type socksReq struct {
	atyp byte   // address type as sent by the client
	host string // target host as sent by the client (IP literal or FQDN)
	port int
}

// testSocksServer is a minimal SOCKS5 server. mapHost lets the "proxy" resolve
// hostnames the client cannot (e.g. fake-proxy-only.invalid → 127.0.0.1).
type testSocksServer struct {
	ln      net.Listener
	mapHost map[string]string
	mu      sync.Mutex
	reqs    []socksReq
}

func newTestSocksServer(t *testing.T, mapHost map[string]string) *testSocksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSocksServer{ln: ln, mapHost: mapHost}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *testSocksServer) Addr() string { return s.ln.Addr().String() }

// requests returns a copy of the CONNECT requests the proxy has seen.
func (s *testSocksServer) requests() []socksReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]socksReq(nil), s.reqs...)
}

func (s *testSocksServer) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(c)
	}
}

func (s *testSocksServer) handleConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	// Greeting: VER(5) NMETHODS METHODS... → reply VER METHOD(0 = no auth).
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}

	// CONNECT request: VER(5) CMD(1) RSV(0) ATYP ADDR PORT.
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 || req[1] != 1 || req[2] != 0 {
		return
	}
	host, ok := readSocksAddr(c, req[3])
	if !ok {
		return
	}
	portB := make([]byte, 2)
	if _, err := io.ReadFull(c, portB); err != nil {
		return
	}
	port := int(portB[0])<<8 | int(portB[1])

	s.mu.Lock()
	s.reqs = append(s.reqs, socksReq{atyp: req[3], host: host, port: port})
	s.mu.Unlock()

	// Resolve the target: an explicit mapping wins; loopback goes to 127.0.0.1
	// because the test origin listens on IPv4 loopback only (the client may
	// legitimately resolve "localhost" to ::1).
	if mapped, ok := s.mapHost[host]; ok {
		host = mapped
	} else if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		host = "127.0.0.1"
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, itoa(port)))
	if err != nil {
		// Connection refused.
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	// Success reply: VER(5) REP(0) RSV(0) ATYP(IPv4) BND.ADDR(0.0.0.0) BND.PORT(0).
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{}) // handshake done; clear the deadline

	// Plain TCP relay in both directions; half-close so both sides see EOF.
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go cp(target, c)
	cp(c, target)
	wg.Wait()
}

// readSocksAddr reads the address field of a SOCKS CONNECT request.
func readSocksAddr(c net.Conn, atyp byte) (string, bool) {
	switch atyp {
	case socksAtypIPv4:
		b := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", false
		}
		return net.IP(b).String(), true
	case socksAtypIPv6:
		b := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", false
		}
		return net.IP(b).String(), true
	case socksAtypFQDN:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", false
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return "", false
		}
		return string(b), true
	}
	return "", false
}

// socksOrigin serves payload with Range support and sets Connection: close so
// the SOCKS relay terminates deterministically once the response is delivered.
func socksOrigin(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.Header().Set("Accept-Ranges", "bytes")
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		start, end, ok := parseClientRange(rng, len(payload))
		if !ok {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if strings.HasSuffix(rng, "-") {
			end = int64(len(payload) - 1) // open-ended range: bytes=N-
		}
		w.Header().Set("Content-Range", "bytes "+itoa(int(start))+"-"+itoa(int(end))+"/"+itoa(len(payload)))
		w.Header().Set("Content-Length", itoa(int(end-start+1)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
}

func TestSocks5ProxyDownload(t *testing.T) {
	payload := []byte("0123456789abcdef-socks5-through-the-proxy")
	origin := socksOrigin(t, payload)
	defer origin.Close()

	px := newTestSocksServer(t, nil)
	c, err := NewClient(ClientConfig{
		Proxy:            "socks5://" + px.Addr(),
		MaxRedirect:      5,
		CheckCertificate: true,
		Timeout:          10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Target the origin by hostname: with socks5 the client must resolve it
	// locally and send an IP address to the proxy.
	originURL, _ := url.Parse(origin.URL)
	target := "http://localhost:" + originURL.Port() + "/"
	rr, err := c.GetRange(context.Background(), target, 0, -1)
	if err != nil {
		t.Fatalf("GetRange through socks5: %v", err)
	}
	defer rr.Resp.Body.Close()
	if !rr.SupportsRange {
		t.Fatal("expected a 206 ranged response through the proxy")
	}
	got, _ := io.ReadAll(rr.Resp.Body)
	if string(got) != string(payload) {
		t.Fatalf("download through socks5: got %q, want %q", got, payload)
	}

	reqs := px.requests()
	if len(reqs) == 0 {
		t.Fatal("proxy saw no CONNECT requests")
	}
	for _, r := range reqs {
		if r.atyp != socksAtypIPv4 && r.atyp != socksAtypIPv6 {
			t.Fatalf("socks5 must send an IP to the proxy (client-side DNS), got atyp=%d host=%q", r.atyp, r.host)
		}
	}
}

func TestSocks5hProxySideDNS(t *testing.T) {
	payload := []byte("socks5h-resolves-at-the-proxy")
	origin := socksOrigin(t, payload)
	defer origin.Close()

	// fake-proxy-only.invalid is unresolvable on the client side; only the
	// proxy knows how to reach it. With socks5h the client must send the
	// hostname and let the proxy resolve it.
	const fakeHost = "fake-proxy-only.invalid"
	px := newTestSocksServer(t, map[string]string{fakeHost: "127.0.0.1"})
	c, err := NewClient(ClientConfig{
		Proxy:       "socks5h://" + px.Addr(),
		MaxRedirect: 5,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	originURL, _ := url.Parse(origin.URL)
	target := "http://" + fakeHost + ":" + originURL.Port() + "/"
	rr, err := c.GetRange(context.Background(), target, 0, int64(len(payload)-1))
	if err != nil {
		t.Fatalf("GetRange through socks5h: %v", err)
	}
	defer rr.Resp.Body.Close()
	got, _ := io.ReadAll(rr.Resp.Body)
	if string(got) != string(payload) {
		t.Fatalf("download through socks5h: got %q, want %q", got, payload)
	}

	found := false
	for _, r := range px.requests() {
		if r.atyp == socksAtypFQDN && r.host == fakeHost {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("proxy never saw a CONNECT for %q (proxy-side DNS not used)", fakeHost)
	}
}

func TestHTTPProxyStillWiredViaTransportProxy(t *testing.T) {
	c, err := NewClient(ClientConfig{Proxy: "http://user:pass@127.0.0.1:3128", MaxRedirect: 5})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", c.HTTP.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("http proxy must keep using Transport.Proxy")
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/file", nil)
	pu, err := tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if pu == nil || pu.Host != "127.0.0.1:3128" || pu.User.Username() != "user" {
		t.Fatalf("unexpected proxy URL %v", pu)
	}
}

func TestSocks5ProxyInvalidURL(t *testing.T) {
	for _, u := range []string{
		"socks5://",           // missing host
		"socks5://:1080",      // empty host
		"socks5://127.0.0.1:0", // port out of range
		"socks5://127.0.0.1:99999", // port out of range (strconv/65535 check in newSocks5Dialer)
		"socks5h://[::1",      // url.Parse rejects
		"socks5://host:notaport", // url.Parse rejects
		"ftp://example.com",   // unsupported scheme
	} {
		if _, err := NewClient(ClientConfig{Proxy: u}); err == nil {
			t.Errorf("NewClient(Proxy=%q): expected an error, got nil", u)
		}
	}
}
