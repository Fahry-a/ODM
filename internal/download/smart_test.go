package download

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"odm/internal/ratelimit"
	"odm/internal/transport"
)

// ---------------------------------------------------------------------------
// ChooseProfile — the smart decision matrix.
// ---------------------------------------------------------------------------

func TestChooseProfile(t *testing.T) {
	cases := []struct {
		name string
		c    ServerCapabilities
		want string
	}{
		{"no range", ServerCapabilities{TotalSize: 1 << 30, SupportsRange: false, Conns: 8, HTTP2Ready: true}, "odm"},
		{"sizeless", ServerCapabilities{TotalSize: -1, SupportsRange: true, Conns: 8, HTTP2Ready: true}, "odm"},
		{"small", ServerCapabilities{TotalSize: 4 << 20, SupportsRange: true, Conns: 8, HTTP2Ready: true}, "odm"},
		{"small boundary 8MiB", ServerCapabilities{TotalSize: 8 << 20, SupportsRange: true, Conns: 8, HTTP2Ready: true}, "aria2c"},
		{"no h2", ServerCapabilities{TotalSize: 1 << 30, SupportsRange: true, Conns: 8, HTTP2Ready: false}, "odm"},
		{"low conns", ServerCapabilities{TotalSize: 1 << 30, SupportsRange: true, Conns: 2, HTTP2Ready: true}, "odm"},
		{"low conns boundary 3", ServerCapabilities{TotalSize: 1 << 30, SupportsRange: true, Conns: 3, HTTP2Ready: true}, "aria2c"},
		{"big+wide → both", ServerCapabilities{TotalSize: 512 << 20, SupportsRange: true, Conns: 6, HTTP2Ready: true}, "both"},
		{"big no both (conns 4)", ServerCapabilities{TotalSize: 512 << 20, SupportsRange: true, Conns: 4, HTTP2Ready: true}, "aria2c"},
		{"default h2", ServerCapabilities{TotalSize: 100 << 20, SupportsRange: true, Conns: 4, HTTP2Ready: true}, "aria2c"},
	}
	for _, tc := range cases {
		got, _ := ChooseProfile(tc.c)
		if got != tc.want {
			t.Errorf("%s: ChooseProfile = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Smart profile end-to-end.
// ---------------------------------------------------------------------------

func buildSmartTask(t *testing.T, dir, url string, conns int) *Task {
	t.Helper()
	h2cli, err := transport.NewClient(transport.ClientConfig{Timeout: 5 * time.Second, HTTP2: true, CheckCertificate: false})
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := ratelimit.New("")
	task := NewTask(TaskID("odm-smart"), url, TaskOptions{
		OutputName:       "out.bin",
		Dir:              dir,
		Retry:            2,
		Timeout:          10 * time.Second,
		ChunkSize:        1 * 1024 * 1024,
		MinSplitSize:     1 * 1024 * 1024,
		Split:            4,
		Profile:          "smart",
		MaxConnPerServer: 8,
	}, h2cli, lim, nil)
	task.SetH2Client(h2cli)
	return task
}

// TestSmartProfile_SmallFallsBackToOdm: a small file over an h2 server must
// choose odm (rule 2) and produce a correct file.
func TestSmartProfile_SmallFallsBackToOdm(t *testing.T) {
	payload := make([]byte, 5*1024*1024) // < 8 MiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(payload))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	task := buildSmartTask(t, dir, srv.URL, 4)
	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("smart small download failed: %v", err)
	}
	if task.opts.Profile != "odm" {
		t.Fatalf("small file must pick odm, got %q", task.opts.Profile)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("small smart produced wrong file: %v", err)
	}
}

// TestSmartProfile_H2BigPicksAria2c: a large file over h2 with decent conns
// must pick aria2c and complete via h2.
func TestSmartProfile_H2BigPicksAria2c(t *testing.T) {
	payload := make([]byte, 100*1024*1024) // >= 8 MiB, < 256 MiB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(payload))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	task := buildSmartTask(t, dir, srv.URL, 4)
	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("smart h2 download failed: %v", err)
	}
	if task.opts.Profile != "aria2c" {
		t.Fatalf("big h2 file must pick aria2c, got %q", task.opts.Profile)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("smart h2 produced wrong file: %v", err)
	}
}

// TestSmartProfile_H1ServerPicksOdm: an h1-only server must resolve to odm
// (no h2) even for a large file.
func TestSmartProfile_H1ServerPicksOdm(t *testing.T) {
	payload := make([]byte, 100*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer srv.Close()

	dir := t.TempDir()
	task := buildSmartTask(t, dir, srv.URL, 4)
	if err := task.Start(context.Background(), 4, nil); err != nil {
		t.Fatalf("smart h1 download failed: %v", err)
	}
	if task.opts.Profile != "odm" {
		t.Fatalf("h1 server must pick odm, got %q", task.opts.Profile)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("smart h1 produced wrong file: %v", err)
	}
}
