// Package transport is the HTTP client layer of ODM. It owns the §5.2
// range-support probe (HEAD → ranged GET → single-stream fallback), the
// redirect limit, custom headers, proxy, and TLS verify toggle. Download
// workers (internal/download) build per-chunk ranged requests on top of the
// Client returned by NewClient.
package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ProbeResult is what the §5.2 probe decides about a URL.
type ProbeResult struct {
	FinalURL      string // URL after following redirects (up to maxRedirect)
	SupportsRange bool   // server accepts ranged GET (206)
	TotalSize     int64  // file size in bytes; -1 if unknown (sizeless stream)
	AcceptRanges  bool   // server advertised "Accept-Ranges: bytes"
	ETag          string // optional, for resume validation
	Filename      string // derived from URL/Content-Disposition (caller may refine)
	StatusCode    int    // last response status used for the size decision
	SingleStream  bool   // true → degrade to one plain sequential GET
}

// ClientConfig maps the handful of Options that govern HTTP behaviour. Keeping
// it a plain struct (not a full *config.Options) keeps transport independent of
// the config package — download tests build a ClientConfig by hand.
type ClientConfig struct {
	UserAgent        string
	Headers          []string // "Key: value"
	Referer          string
	Proxy            string // http/https/socks5/socks5h
	CheckCertificate bool
	MaxRedirect      int
	Timeout          time.Duration // per-request dial+headers timeout; 0 = default
	HTTP2            bool          // true → enable HTTP/2 via ALPN (aria2c/both profiles)
}

// Client wraps http.Client with ODM's identity/redirect settings. Construct via
// NewClient. The underlying http.Client leaves Body handling to callers and is
// reused across probes + chunk downloads (connection pooling).
type Client struct {
	HTTP    *http.Client
	cfg     ClientConfig
	baseHdr http.Header // User-Agent, Referer, custom headers — applied to every request
}

// NewClient builds an HTTP client honouring MaxRedirect, CheckCertificate and
// Proxy. Proxy parsing accepts http:// and https:// (wired through
// Transport.Proxy, i.e. HTTP CONNECT) plus socks5:// and socks5h:// (real
// SOCKS5 via golang.org/x/net/proxy, wired through DialContext — see
// socks5Dialer for the DNS semantics). Unsupported or malformed proxy URLs
// error out here rather than silently falling back to CONNECT tunnelling.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.MaxRedirect < 0 {
		cfg.MaxRedirect = 0
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	tlsConf := &tls.Config{
		InsecureSkipVerify: !cfg.CheckCertificate,
	}
	netDialer := &net.Dialer{
		Timeout:   cfg.Timeout,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		DialContext:           netDialer.DialContext,
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
		DisableCompression:    false,
		// Generous pooling for multi-connection downloads.
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.HTTP2 {
		// h2 enabled (aria2c / both-region2 / smart profiles): let net/http
		// auto-register HTTP/2 (TLSNextProto nil). https:// hosts negotiate h2
		// via ALPN; an h2-less server simply stays HTTP/1.1 — graceful
		// degradation for free. Streams are multiplexed over ONE TCP
		// connection; the stdlib client respects the server's
		// SETTINGS_MAX_CONCURRENT_STREAMS by queueing internally, so N worker
		// requests share a single connection — this is exactly the aria2c
		// model where -c means concurrent h2 streams, not TCP connections.
		tr.ForceAttemptHTTP2 = true
	} else {
		// ponytail: ForceAttemptHTTP2=false + empty TLSNextProto = unconditional h2
		// disable. ODM's value prop is multi-connection aggregation over HTTP/1.1.
		// If the server negotiated h2 via ALPN, Go would collapse N worker requests
		// into 1 TCP connection with N streams — the Balancer's N-connection
		// allocation becomes meaningless and the user gets zero benefit from -c
		// without any warning. Empty TLSNextProto prevents the client from
		// advertising h2 in the ALPN list at all.
		tr.ForceAttemptHTTP2 = false
		tr.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	}
	if cfg.Proxy != "" {
		px, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		switch px.Scheme {
		case "http", "https":
			tr.Proxy = http.ProxyURL(px)
		case "socks5", "socks5h":
			// Transport.Proxy only speaks HTTP CONNECT; real SOCKS5 needs a
			// custom dialer. golang.org/x/net/proxy speaks the SOCKS5 wire
			// protocol (RFC 1928) directly. DNS semantics: socks5 resolves the
			// target locally and sends an IP to the proxy; socks5h sends the
			// hostname and lets the proxy resolve it (see socks5Dialer).
			sd, err := newSocks5Dialer(px, netDialer)
			if err != nil {
				return nil, err
			}
			tr.DialContext = sd.DialContext
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q (use http/https/socks5/socks5h)", px.Scheme)
		}
	}

	// Redirect policy: follow up to MaxRedirect hops; error past that with a
	// clear message. A nil policy would mean "follow 10" — we want explicitness.
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) > cfg.MaxRedirect {
			return fmt.Errorf("max redirects (%d) exceeded", cfg.MaxRedirect)
		}
		return nil
	}

	hc := &http.Client{
		Transport:     tr,
		CheckRedirect: checkRedirect,
		Timeout:       0, // we manage timeouts via context per-chunk (streaming reads are long)
	}

	c := &Client{HTTP: hc, cfg: cfg}
	if err := c.initBaseHeaders(); err != nil {
		return nil, err
	}
	return c, nil
}

// socks5Dialer wraps golang.org/x/net/proxy's SOCKS5 dialer as an
// http.Transport.DialContext hook. It implements the two DNS semantics:
//
//	socks5  — client-side DNS: the target hostname is resolved locally and the
//	          resulting IP is handed to the proxy (ATYP IPv4/IPv6).
//	socks5h — proxy-side DNS: the hostname is sent to the proxy unchanged
//	          (ATYP FQDN), so names only the proxy can resolve still work.
//
// x/net/proxy's FromURL treats both schemes identically (always forwarding the
// hostname as given — i.e. socks5h behaviour), so socks5 needs the extra local
// resolution step in DialContext; socks5h can use the x/net dialer directly.
type socks5Dialer struct {
	d       proxy.Dialer // underlying x/net SOCKS5 dialer
	resolve bool         // true → socks5: resolve target hostname locally
	timeout time.Duration
}

// newSocks5Dialer validates the socks5/socks5h URL and builds the x/net dialer
// on top of forward (a *net.Dialer carrying the ClientConfig Timeout/KeepAlive,
// used for the TCP hop to the proxy itself).
func newSocks5Dialer(px *url.URL, forward *net.Dialer) (*socks5Dialer, error) {
	if px.Hostname() == "" {
		return nil, fmt.Errorf("proxy: %s URL %q has no host", px.Scheme, px)
	}
	if p := px.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("proxy: %s URL %q has invalid port %q", px.Scheme, px, p)
		}
	}
	d, err := proxy.FromURL(px, forward)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	return &socks5Dialer{d: d, resolve: px.Scheme == "socks5", timeout: forward.Timeout}, nil
}

// DialContext is the http.Transport.DialContext hook. When the caller's context
// carries no deadline it applies cfg.Timeout to the whole dial (local DNS,
// TCP to the proxy, SOCKS handshake), matching the plain net.Dialer behaviour.
func (s *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if _, ok := ctx.Deadline(); !ok && s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	if s.resolve {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("proxy socks5: %w", err)
		}
		if net.ParseIP(host) == nil {
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("proxy socks5: resolve %q: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("proxy socks5: no addresses for %q", host)
			}
			address = net.JoinHostPort(ips[0].IP.String(), port)
		}
	}
	cd, ok := s.d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("proxy socks5: dialer does not support context dialing")
	}
	return cd.DialContext(ctx, network, address)
}

// initBaseHeaders parses cfg.Headers into a reusable header set and stamps the
// User-Agent + Referer. Each request cloned from applyBaseHeaders() will carry
// these; per-request additions (Range, etc.) layer on top.
func (c *Client) initBaseHeaders() error {
	c.baseHdr = http.Header{}
	if c.cfg.UserAgent != "" {
		c.baseHdr.Set("User-Agent", c.cfg.UserAgent)
	}
	if c.cfg.Referer != "" {
		c.baseHdr.Set("Referer", c.cfg.Referer)
	}
	for _, h := range c.cfg.Headers {
		key, val, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("invalid --header (want 'Key: value'): %q", h)
		}
		c.baseHdr.Add(strings.TrimSpace(key), strings.TrimSpace(val))
	}
	return nil
}

// NewGetRequest builds a plain GET request to rawURL with ODM's base headers
// applied (User-Agent, Referer, custom -H headers). Exposed so the download
// engine can fire the single-stream fallback (§11.2) reusing the same headers.
func (c *Client) NewGetRequest(ctx context.Context, rawURL string) (*http.Request, error) {
	return c.newRequest(ctx, http.MethodGet, rawURL)
}

// newRequest builds a request of the given method to rawURL with the base
// headers applied. Caller adds Range/etc. on the returned request.
func (c *Client) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range c.baseHdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// SkipBody drains r up to a small limit then closes it so the connection can
// be returned to the pool. Call this for probe responses whose body you don't
// need.
func SkipBody(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 1024))
	_ = r.Close()
}

// contentDispositionFilename extracts the filename parameter of a
// Content-Disposition header via the stdlib (handles quoted, unquoted and
// RFC 5987 filename*= forms).
func contentDispositionFilename(v string) string {
	if v == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(v); err == nil {
		return params["filename"]
	}
	return ""
}

// Probe runs the §5.2 three-step chain to determine size + range support for
// rawURL. It never returns both SupportsRange=false *and* TotalSize known ≠ -1
// in a way that contradicts: a server that reports size but refuses ranges sets
// SingleStream=true and SupportsRange=false; a server that can't tell us the
// size at all sets TotalSize=-1 and SingleStream=true.
func (c *Client) Probe(ctx context.Context, rawURL string) (*ProbeResult, error) {
	pr := &ProbeResult{FinalURL: rawURL, TotalSize: -1}

	// Step 1: HEAD. Many servers block it or misreport — fall through on any
	// non-2xx (or transport error) to step 2.
	if r, err := c.doHead(ctx, rawURL); err == nil && r != nil {
		pr.FinalURL = r.Request.URL.String()
		pr.StatusCode = r.StatusCode
		pr.ETag = r.Header.Get("ETag")
		pr.Filename = contentDispositionFilename(r.Header.Get("Content-Disposition"))
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			if cl := r.Header.Get("Content-Length"); cl != "" {
				if v, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
					pr.TotalSize = v
				}
			}
			pr.AcceptRanges = strings.EqualFold(r.Header.Get("Accept-Ranges"), "bytes")
			if cl := r.Header.Get("Content-Range"); cl != "" {
				if v := parseRangeTotal(cl); v >= 0 {
					pr.TotalSize = v
				}
				pr.AcceptRanges = true
			}
			SkipBody(r.Body)
			if pr.AcceptRanges && pr.TotalSize > 0 {
				pr.SupportsRange = true
				return pr, nil
			}
		} else {
			SkipBody(r.Body)
		}
	}

	// Step 2: GET with Range: bytes=0-0. A 206 → ranges supported, total from
	// Content-Range. A 200 → no range support (but we may still learn the size
	// from Content-Length): single-stream fallback.
	r, err := c.doRangeProbe(ctx, rawURL)
	if err != nil {
		// Could be a server that refuses ranged GET entirely; try a plain GET
		// for size only as a last resort before declaring sizeless single-stream.
		if pr2, perr := c.doPlainSizeProbe(ctx, rawURL); perr == nil {
			mergeSize(pr, pr2)
			pr.SingleStream = true
			return pr, nil
		}
		return nil, fmt.Errorf("probe failed for %s: %w", rawURL, err)
	}
	pr.FinalURL = r.Request.URL.String()
	pr.StatusCode = r.StatusCode
	pr.ETag = r.Header.Get("ETag")
	if pr.Filename == "" {
		pr.Filename = contentDispositionFilename(r.Header.Get("Content-Disposition"))
	}

	switch {
	case r.StatusCode == http.StatusPartialContent:
		pr.SupportsRange = true
		pr.AcceptRanges = true
		if cr := r.Header.Get("Content-Range"); cr != "" {
			if v := parseRangeTotal(cr); v >= 0 {
				pr.TotalSize = v
			}
		}
	case r.StatusCode >= 200 && r.StatusCode < 300:
		// No range support: single-stream. Still capture size if present.
		pr.SingleStream = true
		if cl := r.Header.Get("Content-Length"); cl != "" {
			if v, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
				pr.TotalSize = v
			}
		}
	default:
		SkipBody(r.Body)
		return nil, fmt.Errorf("probe: unexpected status %d for %s", r.StatusCode, rawURL)
	}
	// We requested 1 byte; read nothing more than needed before closing.
	SkipBody(r.Body)
	return pr, nil
}

func (c *Client) doHead(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodHead, rawURL)
	if err != nil {
		return nil, err
	}
	return c.HTTP.Do(req)
}

func (c *Client) doRangeProbe(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	return c.HTTP.Do(req)
}

// doPlainSizeProbe fires an ordinary GET solely to learn Content-Length; the
// body is limited to 1 byte then dropped. Used only when HEAD *and* ranged GET
// both fail (e.g. server 403s ranged requests but serves the whole file).
func (c *Client) doPlainSizeProbe(ctx context.Context, rawURL string) (*ProbeResult, error) {
	req, err := c.newRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, err
	}
	r, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer SkipBody(r.Body)
	pr := &ProbeResult{FinalURL: r.Request.URL.String(), StatusCode: r.StatusCode}
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		if cl := r.Header.Get("Content-Length"); cl != "" {
			if v, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
				pr.TotalSize = v
			}
		}
		return pr, nil
	}
	return nil, fmt.Errorf("plain probe: status %d", r.StatusCode)
}

func mergeSize(dst, src *ProbeResult) {
	if dst.TotalSize < 0 && src.TotalSize > 0 {
		dst.TotalSize = src.TotalSize
	}
	dst.FinalURL = src.FinalURL
	dst.StatusCode = src.StatusCode
}

// parseRangeTotal extracts the total-byte field of a "Content-Range: bytes 0-0/12345".
func parseRangeTotal(cr string) int64 {
	// form: bytes 0-0/12345  (or  */12345)
	_, after, ok := strings.Cut(cr, "/")
	if !ok {
		return -1
	}
	after = strings.TrimSpace(after)
	if after == "*" {
		return -1
	}
	v, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// GetRange issues a ranged GET for [start, end] inclusive (end<0 means
// end-of-file). Returns the open response; the caller owns Body. On a server
// that ignores Range (responds 200), the caller should treat the whole body as
// byte 0 onward (single-stream degeneration, §11.2).
func (c *Client) GetRange(ctx context.Context, rawURL string, start, end int64) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, err
	}
	if end < 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}
	r, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if r.StatusCode != http.StatusPartialContent && r.StatusCode != http.StatusOK {
		SkipBody(r.Body)
		return nil, fmt.Errorf("GetRange: status %d for %s", r.StatusCode, rawURL)
	}
	return r, nil
}
