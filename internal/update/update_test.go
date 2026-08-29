package update

import (
	"testing"
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
