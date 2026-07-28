// Package transport is the HTTP client layer of ODM. It owns the §5.2
// range-support probe (HEAD → ranged GET → single-stream fallback), the
// redirect limit, custom headers, proxy, and TLS verify toggle. Download
// workers (internal/download) build per-chunk ranged requests on top of the
// Client returned by NewClient.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	Proxy            string // http/https/socks5
	CheckCertificate bool
	MaxRedirect      int
	Timeout          time.Duration // per-request dial+headers timeout; 0 = default
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
// Proxy. Proxy parsing accepts http://, https:// and socks5:// (via Go's
// net/http URL-based proxy support for HTTP schemes; socks5 needs a custom
// Dialer — we wire it through the Transport).
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
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.Timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
		ForceAttemptHTTP2:     true, // degrade gracefully against HTTP/2-only servers (PRD §15)
		DisableCompression:    false,
		// Generous pooling for multi-connection downloads.
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.Proxy != "" {
		px, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		switch px.Scheme {
		case "http", "https":
			tr.Proxy = http.ProxyURL(px)
		case "socks5":
			// Transport.Proxy only supports HTTP CONNECT proxies; for socks5
			// net/http can dial through a *net.Dialer from golang.org/x/net,
			// but to avoid an extra dep we set Proxy to http.ProxyURL for
			// socks5 too and rely on Go's built-in socks5-by-CONNECT fallback
			// when present; otherwise error early so the user knows to use
			// an http(s) proxy.
			tr.Proxy = http.ProxyURL(px)
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q (use http/https/socks5)", px.Scheme)
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

// parseContentDisposition extracts the filename parameter from a
// Content-Disposition response header. Supports both quoted and unquoted
// filename= values per RFC 6266:
//
//	Content-Disposition: attachment; filename="foo.bin"
//	Content-Disposition: attachment; filename=foo.bin
//	Content-Disposition: attachment; filename*=UTF-8''foo.bin
//
// Returns "" when no usable filename is found.
func parseContentDisposition(v string) string {
	if v == "" {
		return ""
	}
	// Try filename*= (RFC 5987) first — it takes precedence.
	_, after, ok := strings.Cut(v, "filename*=")
	if ok {
		// Skip charset and language: charset'lang'value
		if sq := strings.IndexByte(after, '\''); sq >= 0 {
			after = after[sq+1:]
			if sq2 := strings.IndexByte(after, '\''); sq2 >= 0 {
				after = after[sq2+1:]
			}
		}
		if after != "" {
			after = strings.TrimSpace(after)
			if len(after) > 0 && after[len(after)-1] == ';' {
				after = after[:len(after)-1]
			}
			return after
		}
	}
	// Try filename= (RFC 6266).
	_, after, ok = strings.Cut(v, "filename=")
	if !ok {
		return ""
	}
	after = strings.TrimSpace(after)
	if len(after) > 0 && after[0] == '"' {
		// Quoted: filename="foo.bin"
		end := strings.IndexByte(after[1:], '"')
		if end >= 0 {
			return after[1 : 1+end]
		}
	}
	// Unquoted: filename=foo.bin  (up to ';' or end)
	if i := strings.IndexByte(after, ';'); i >= 0 {
		after = after[:i]
	}
	after = strings.TrimSpace(after)
	if after != "" {
		return after
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
		pr.Filename = parseContentDisposition(r.Header.Get("Content-Disposition"))
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
		pr.Filename = parseContentDisposition(r.Header.Get("Content-Disposition"))
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
// that ignores Range (responds 200), SupportsRange on the response is false and
// the caller should treat the whole body as byte 0 onward (single-stream
// degeneration, §11.2).
type RangeResult struct {
	Resp          *http.Response
	SupportsRange bool  // 206?
	Offset        int64 // byte offset the returned body corresponds to
	TotalSize     int64
}

func (c *Client) GetRange(ctx context.Context, rawURL string, start, end int64) (*RangeResult, error) {
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
	rr := &RangeResult{Resp: r, Offset: start}
	rr.SupportsRange = r.StatusCode == http.StatusPartialContent
	// Reconcile actual offset from Content-Range if present (server may have
	// adjusted the start).
	if cr := r.Header.Get("Content-Range"); cr != "" {
		if st, _, tot, ok := parseContentRange(cr); ok {
			rr.Offset = st
			rr.TotalSize = tot
		}
	}
	if !rr.SupportsRange {
		rr.Offset = 0 // whole body is byte 0 onward
		if cl := r.Header.Get("Content-Length"); cl != "" {
			if v, _ := strconv.ParseInt(cl, 10, 64); v > 0 {
				rr.TotalSize = v
			}
		}
	}
	return rr, nil
}

// parseContentRange parses "bytes <start>-<end>/<total>".
func parseContentRange(cr string) (start, end, total int64, ok bool) {
	_, spec, found := strings.Cut(cr, " ")
	if !found {
		return 0, 0, 0, false
	}
	rng, tot, hasTot := strings.Cut(spec, "/")
	s, e, rngOk := strings.Cut(rng, "-")
	if !rngOk {
		return 0, 0, 0, false
	}
	st, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	en, err := strconv.ParseInt(strings.TrimSpace(e), 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	var tt int64 = -1
	if hasTot {
		tt, _ = strconv.ParseInt(strings.TrimSpace(tot), 10, 64)
	}
	return st, en, tt, true
}

// ErrMaxRedirects is returned when the redirect chain exceeds MaxRedirect.
var ErrMaxRedirects = errors.New("max redirects exceeded")
