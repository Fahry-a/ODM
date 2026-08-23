package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// TestMirrorRotation pins --mirror: chunks rotate across the primary URL and
// every mirror (each source serves the same bytes, so the assembled file is
// identical whichever source fed each chunk).
func TestMirrorRotation(t *testing.T) {
	payload := payload4K()
	var mu sync.Mutex
	seen := map[string]int{}

	newSrc := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Accept-Ranges", "bytes")
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", itoaS(len(payload)))
				w.WriteHeader(http.StatusOK)
				return
			}
			mu.Lock()
			seen[name]++
			mu.Unlock()
			start, end, ok := parseClientRangeS(r.Header.Get("Range"), len(payload))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", "bytes "+itoaS(int(start))+"-"+itoaS(int(end))+"/"+itoaS(len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start : end+1])
		}))
	}
	primary := newSrc("primary")
	defer primary.Close()
	m1 := newSrc("m1")
	defer m1.Close()

	dir := t.TempDir()
	cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("t"), primary.URL, TaskOptions{
		OutputName: "out.bin",
		Dir:        dir,
		Retry:      2,
		RetryWait:  time.Millisecond,
		Timeout:    5 * time.Second,
		ChunkSize:  1024, // 4 chunks → with rotation both sources should serve
		Mirrors:    []string{m1.URL},
	}, cli, lim, nil)
	task.SetProbe(&transport.ProbeResult{FinalURL: primary.URL, SupportsRange: true, TotalSize: int64(len(payload)), Filename: "out.bin"})
	task.SetProfile("odm")
	if err := task.Start(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dir + "/out.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("file %d bytes, want %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d mismatch — mirror served different content?", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["m1"] == 0 {
		t.Fatalf("mirror never used: hits=%v", seen)
	}
	t.Logf("chunk hits: primary=%d m1=%d", seen["primary"], seen["m1"])
}
