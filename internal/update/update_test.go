package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.7.0", "1.7.0", 0},
		{"1.7.0", "1.8.0", -1},
		{"1.8.0", "1.7.0", 1},
		{"1.7.0", "1.7.1", -1},
		{"1.7.1", "1.7.0", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.9.9", "2.0.0", -1},
		{"10.20.30", "10.20.31", -1},
		{"v1.7.0", "1.7.0", 0},
		{"odm/1.7.0", "1.7.0", 0},
		{"1.7.0", "v1.8.0", -1},
	}
	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"1.7.0", [3]int{1, 7, 0}},
		{"v2.0.1", [3]int{2, 0, 1}},
		{"odm/1.7.0", [3]int{1, 7, 0}},
		{"10.20.30", [3]int{10, 20, 30}},
		{"0.0.0", [3]int{0, 0, 0}},
	}
	for _, tt := range tests {
		got := parseSemver(tt.input)
		if got != tt.want {
			t.Errorf("parseSemver(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// stubCurl installs a fake curl on PATH that prints the given stdout (and
// exits with exitCode). Hint/CheckLatest shell out to curl, so this is the
// hermetic seam — no network is involved.
func stubCurl(t *testing.T, stdout string, exitCode int) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n"
	if exitCode == 0 {
		script += "printf '%s' '" + strings.ReplaceAll(stdout, "'", "'\\''") + "'\n"
	} else {
		script += "exit " + strconv.Itoa(exitCode) + "\n"
	}
	path := filepath.Join(bin, "curl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCheckLatest_ParsesTag(t *testing.T) {
	stubCurl(t, `{"tag_name":"v9.9.9"}`, 0)
	ver, url, err := CheckLatest()
	if err != nil {
		t.Fatal(err)
	}
	if ver != "9.9.9" {
		t.Fatalf("ver = %q, want 9.9.9", ver)
	}
	for _, want := range []string{"v9.9.9", "odm_9.9.9_"} {
		if !strings.Contains(url, want) {
			t.Fatalf("download URL %q missing %q", url, want)
		}
	}
}

func TestCheckLatest_CurlFailure(t *testing.T) {
	stubCurl(t, "", 22)
	if _, _, err := CheckLatest(); err == nil {
		t.Fatal("curl failure must surface as an error")
	}
}

func TestCheckLatest_GarbageResponse(t *testing.T) {
	stubCurl(t, "<html>not json</html>", 0)
	if _, _, err := CheckLatest(); err == nil {
		t.Fatal("an unparseable response must surface as an error")
	}
}

func TestHint_UpdateAvailable(t *testing.T) {
	stubCurl(t, `{"tag_name":"v9.9.9"}`, 0)
	home := t.TempDir()
	t.Setenv("HOME", home)
	hint := Hint("1.0.0")
	if !strings.Contains(hint, "9.9.9") {
		t.Fatalf("hint = %q, want it to mention 9.9.9", hint)
	}
	// The result must be cached for 24h (file exists after a fresh check).
	cache := filepath.Join(home, hintCacheDir, hintCacheFile)
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestHint_UpToDate(t *testing.T) {
	stubCurl(t, `{"tag_name":"v0.0.1"}`, 0)
	t.Setenv("HOME", t.TempDir())
	if hint := Hint("9.9.9"); hint != "" {
		t.Fatalf("hint = %q, want empty when up to date", hint)
	}
}

// TestHint_FreshCacheSkipsNetwork: a 24h-old-or-newer cache entry must be
// honored without contacting the network at all (curl is made to fail).
func TestHint_FreshCacheSkipsNetwork(t *testing.T) {
	stubCurl(t, "", 22) // network down — a fetch would error out and yield ""
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, hintCacheDir, hintCacheFile)
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(fmt.Sprintf("%d\n9.9.9\n", time.Now().Unix())), 0o644); err != nil {
		t.Fatal(err)
	}
	if hint := Hint("1.0.0"); !strings.Contains(hint, "9.9.9") {
		t.Fatalf("hint = %q, want cached update notice", hint)
	}
}

func TestDetectInstallMethod(t *testing.T) {
	// Just verify it doesn't panic and returns a valid method
	m := DetectInstallMethod()
	switch m {
	case MethodSelf, MethodAUR, MethodManual:
		// ok
	default:
		t.Errorf("DetectInstallMethod() returned invalid method: %d", m)
	}
}

func TestInstallMethodString(t *testing.T) {
	tests := []struct {
		m    InstallMethod
		want string
	}{
		{MethodSelf, "self"},
		{MethodAUR, "aur"},
		{MethodManual, "manual"},
		{InstallMethod(99), "unknown"},
	}
	for _, tt := range tests {
		got := tt.m.String()
		if got != tt.want {
			t.Errorf("InstallMethod(%d).String() = %q, want %q", tt.m, got, tt.want)
		}
	}
}
