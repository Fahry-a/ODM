package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"odm/internal/download"
)

func TestBar_FullEaten(t *testing.T) {
	got := Bar(100, 100, 10)
	if got != "----------" {
		t.Fatalf("100%% must be fully dashes, got %q", got)
	}
}

func TestBar_MidProgress(t *testing.T) {
	// 50% of 10 cells → 5 eaten dashes, then face, then 4 dots.
	got := Bar(50, 100, 10)
	want := "-----c0000"
	wantR := strings.NewReplacer("0", "o")
	if got != wantR.Replace(want) {
		t.Fatalf("50%% want %q got %q", wantR.Replace(want), got)
	}
}

func TestBar_SizelessIndeterminate(t *testing.T) {
	got := Bar(0, -1, 10)
	// half dashes + face + rest dots.
	if !strings.Contains(got, "c") || !strings.Contains(got, "o") {
		t.Fatalf("sizeless bar must contain face+dots, got %q", got)
	}
}

func TestRenderTaskLine_Format(t *testing.T) {
	v := download.ProgressView{
		Filename: "linux-cachyos", TotalSize: 120 << 20, Speed: 25 << 20,
		BytesDone: 86 << 20, Connections: 16, State: download.StateActive, ETA: 5 * time.Second,
	}
	line := RenderTaskLine(v, false)
	for _, want := range []string{"linux-cachyos", "MiB", "[x16]", "71%", "%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line missing %q: %s", want, line)
		}
	}
}

func TestRenderSummary(t *testing.T) {
	s := RenderSummary(3, 16, 44_000_000, 32*time.Second, false)
	if !strings.Contains(s, "3/16") || !strings.Contains(s, "00:32") {
		t.Fatalf("summary wrong: %s", s)
	}
}

func TestConfirmAsk(t *testing.T) {
	cases := map[string]bool{
		"y\n":   true,
		"\n":    true, // default yes
		"yes\n": true,
		"n\n":   false,
		"No\n":  false,
	}
	for in, want := range cases {
		var out bytes.Buffer
		got, err := ConfirmAsk(strings.NewReader(in), &out, "Continue? [Y/n] ")
		if err != nil {
			t.Fatalf("in=%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("in=%q: want %v got %v", in, want, got)
		}
	}
}

func TestConfirmSingle(t *testing.T) {
	var out bytes.Buffer
	ok, err := ConfirmSingle(strings.NewReader("y\n"), &out, "file.tar.zst", "/dest/file.tar.zst", 120<<20, 16)
	if err != nil || !ok {
		t.Fatalf("should confirm yes, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(out.String(), "linux") && !strings.Contains(out.String(), "file.tar.zst") {
		t.Fatalf("prompt missing details: %s", out.String())
	}
}
