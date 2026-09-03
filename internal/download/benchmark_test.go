package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- benchmark test servers --------------------------------------------------

// benchRangeServer serves a payload with full Range support (HEAD + 206).
func benchRangeServer(payload []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", itoaB(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", itoaB(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		start, end, ok := parseBenchRange(rng, len(payload))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes "+itoaB(int(start))+"-"+itoaB(int(end))+"/"+itoaB(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
}

// benchNoRangeServer serves a payload ignoring Range headers (always 200).
func benchNoRangeServer(payload []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoaB(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
}

func itoaB(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func parseBenchRange(h string, total int) (start, end int64, ok bool) {
	_, rng, found := cutB(h, "=")
	if !found {
		return 0, 0, false
	}
	sstr, estr, found := cutB(rng, "-")
	if !found {
		return 0, 0, false
	}
	s := int64(atoiB(sstr))
	e := int64(atoiB(estr))
	if e < 0 || e >= int64(total) {
		e = int64(total) - 1
	}
	return s, e, true
}

func cutB(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func atoiB(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// --- benchmark payload generators --------------------------------------------

func benchPayload64K() []byte {
	p := make([]byte, 64*1024)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

func benchPayload1M() []byte {
	p := make([]byte, 1<<20)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

func benchPayload16M() []byte {
	p := make([]byte, 16<<20)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

// --- benchmark runner --------------------------------------------------------

type benchCase struct {
	profile   string
	conns     int
	chunkSize int64
	fileSize  string
	payload   []byte
}

func benchDownload(b *testing.B, bc benchCase) {
	b.Helper()

	srv := benchRangeServer(bc.payload)
	defer srv.Close()

	dir := b.TempDir()

	for i := 0; i < b.N; i++ {
		m, err := NewManager(ExecOptions{
			Dir:              dir,
			OutFile:          "bench.bin",
			Connections:      bc.conns,
			Retry:            0,
			Continue:         false,
			ChunkSize:        bc.chunkSize,
			Timeout:          30 * time.Second,
			CheckCert:        true,
			Profile:          bc.profile,
			Split:            5,
			MinSplitSize:     20 << 20,
			MaxConnPerServer: 8,
		}, nil)
		if err != nil {
			b.Fatalf("NewManager: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := m.Run(ctx, srv.URL, bc.conns); err != nil {
			cancel()
			b.Fatalf("download: %v", err)
		}
		cancel()
	}

	b.SetBytes(int64(len(bc.payload)))
}

// --- benchmarks -------------------------------------------------------------

func BenchmarkDownload(b *testing.B) {
	sizes := []struct {
		name    string
		payload []byte
	}{
		{"64K", benchPayload64K()},
		{"1M", benchPayload1M()},
		{"16M", benchPayload16M()},
	}

	profiles := []string{"odm", "aria2c", "both", "smart"}
	connsList := []int{1, 4, 8}
	chunkSizes := []struct {
		name string
		size int64
	}{
		{"256K", 256 * 1024},
		{"1M", 1 << 20},
		{"4M", 4 << 20},
	}

	for _, sz := range sizes {
		for _, profile := range profiles {
			for _, conns := range connsList {
				for _, cs := range chunkSizes {
					name := sz.name + "/" + profile + "/c" + itoaB(conns) + "/" + cs.name
					b.Run(name, func(b *testing.B) {
						benchDownload(b, benchCase{
							profile:   profile,
							conns:     conns,
							chunkSize: cs.size,
							fileSize:  sz.name,
							payload:   sz.payload,
						})
					})
				}
			}
		}
	}
}

// BenchmarkDownload_NoRange tests single-stream fallback against a server that
// ignores Range headers — the engine must still produce a correct file.
func BenchmarkDownload_NoRange(b *testing.B) {
	payload := benchPayload1M()
	srv := benchNoRangeServer(payload)
	defer srv.Close()

	dir := b.TempDir()

	for i := 0; i < b.N; i++ {
		m, err := NewManager(ExecOptions{
			Dir:         dir,
			OutFile:     "bench-nor.bin",
			Connections: 4,
			Retry:       0,
			Continue:    false,
			ChunkSize:   1 << 20,
			Timeout:     30 * time.Second,
			CheckCert:   true,
			Profile:     "odm",
		}, nil)
		if err != nil {
			b.Fatalf("NewManager: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := m.Run(ctx, srv.URL, 4); err != nil {
			cancel()
			b.Fatalf("download: %v", err)
		}
		cancel()
	}

	b.SetBytes(int64(len(payload)))
}

// BenchmarkDownload_ProfileComparison benchmarks all four profiles on the same
// 16 MiB payload with 8 connections and 1 MiB chunks — the canonical comparison
// from the audit handoff.
func BenchmarkDownload_ProfileComparison(b *testing.B) {
	payload := benchPayload16M()
	srv := benchRangeServer(payload)
	defer srv.Close()

	dir := b.TempDir()

	for _, profile := range []string{"odm", "aria2c", "both", "smart"} {
		b.Run(profile, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				m, err := NewManager(ExecOptions{
					Dir:              dir,
					OutFile:          "bench-cmp.bin",
					Connections:      8,
					Retry:            0,
					Continue:         false,
					ChunkSize:        1 << 20,
					Timeout:          60 * time.Second,
					CheckCert:        true,
					Profile:          profile,
					Split:            5,
					MinSplitSize:     20 << 20,
					MaxConnPerServer: 8,
				}, nil)
				if err != nil {
					b.Fatalf("NewManager: %v", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				if err := m.Run(ctx, srv.URL, 8); err != nil {
					cancel()
					b.Fatalf("download: %v", err)
				}
				cancel()
			}
			b.SetBytes(int64(len(payload)))
		})
	}
}
