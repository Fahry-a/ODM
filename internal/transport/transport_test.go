package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
