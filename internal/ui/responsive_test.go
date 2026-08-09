package ui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"odm/internal/download"
)

// setTerm pins the Renderer's terminal size (via the sizeFn seam) so tests
// render into a deterministic "terminal" regardless of the host.
func (r *Renderer) setTerm(width, height int) {
	r.sizeFn = func(io.Writer) (int, int) { return width, height }
}

// ---------------------------------------------------------------------------
// Narrow-terminal degradation: the task line + summary must always fit the
// terminal width and never wrap (a wrapped row desyncs the cursor-up tally).
// ---------------------------------------------------------------------------

func TestLayoutFor_Tiers(t *testing.T) {
	cases := []struct {
		term   int
		barW   int
		layout lineLayout
	}{
		{120, 30, layoutFull}, // full layout
		{100, 30, layoutFull},
		{96, 30, layoutFull},       // full layout floor
		{80, 20, layoutNoSpeedETA}, // mid tier: speed/ETA dropped, bar shrinks
		{66, 20, layoutNoSpeedETA}, // mid tier floor
		{60, 10, layoutNameBarPct}, // small tier: bar at floor
		{45, 10, layoutNameBarPct}, // small tier floor
		{40, 0, layoutNamePct},     // barless: name + pct only
		{30, 0, layoutNamePct},
	}
	for _, tc := range cases {
		barW, _, minW, layout := layoutFor(tc.term)
		if barW != tc.barW || layout != tc.layout {
			t.Errorf("layoutFor(%d) = barW %d layout %v, want barW %d layout %v",
				tc.term, barW, layout, tc.barW, tc.layout)
		}
		if minW < 0 || minW > tc.term {
			t.Errorf("layoutFor(%d): minW %d must fit in %d cols", tc.term, minW, tc.term)
		}
	}
}

func TestRenderTaskLine_FitsNarrowWidths(t *testing.T) {
	v := download.ProgressView{
		Filename: "linux-cachyos-6.9.1-x86_64.iso", TotalSize: 2 << 30, Speed: 50 << 20,
		BytesDone: 1 << 30, Connections: 16, State: download.StateActive, ETA: 30 * time.Second,
	}
	for _, width := range []int{120, 80, 60, 40, 30, 20} {
		barW, nameW, _, layout := layoutFor(width)
		line := renderTaskLine(v, false, -1, 0, nameW, barW, layout)
		if d := displayWidth(line); d > width {
			t.Errorf("width %d: line %d cells too wide: %q", width, d, line)
		}
		if layout == layoutNamePct && strings.Contains(line, "[") {
			t.Errorf("width %d: barless layout must have no bracket: %q", width, line)
		}
		if !strings.Contains(line, "%") {
			t.Errorf("width %d: line missing percent: %q", width, line)
		}
	}
}

func TestRenderTaskLine_CompactKeepsPctAndName(t *testing.T) {
	v := download.ProgressView{
		Filename: "file.tar.zst", TotalSize: 100, Speed: 5,
		BytesDone: 50, Connections: 4, State: download.StateActive, ETA: time.Second,
	}
	line := renderTaskLine(v, true, -1, 0, 6, 0, layoutNamePct)
	if !strings.Contains(line, "50%") {
		t.Fatalf("barless line must show percent: %q", line)
	}
	if !strings.Contains(line, "fil...") {
		t.Fatalf("barless line must show the truncated name: %q", line)
	}
	if !strings.Contains(line, "\x1b[") {
		t.Fatalf("colored compact line must contain ANSI codes: %q", line)
	}
}

// ---------------------------------------------------------------------------
// Row budget: a batch taller than the terminal must not overflow the screen
// (the cursor-up count would then mismatch and corrupt the next redraw).
// ---------------------------------------------------------------------------

func TestFrame_RowBudgetCapsTaskList(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, false)
	r.tty = true
	r.useColor = false
	r.setTerm(60, 5) // tiny terminal: width 60, height 5 rows

	view := make([]download.ProgressView, 20)
	for i := range view {
		view[i] = download.ProgressView{
			ID: download.TaskID(rune('a' + i)), Filename: "f", TotalSize: 100,
			BytesDone: 10, Speed: 1, ETA: time.Second, State: download.StateActive,
		}
	}
	r.Frame(view, nil)

	chunk := out.String()
	lines := strings.Count(chunk, "\n")
	// 5 rows budget → 3 task lines + summary + "N more…" — never more than
	// the terminal height, and the "more" line is inside the budget now.
	if lines > 5 {
		t.Fatalf("frame wrote %d rows on a %d-row terminal; must cap at height\n%s", lines, 5, chunk)
	}
	if lines != 5 {
		t.Fatalf("frame must fill the 5-row budget (3 tasks + summary + more), wrote %d\n%s", lines, chunk)
	}
	// Hidden tasks must be acknowledged.
	if !strings.Contains(chunk, "more") {
		t.Fatalf("row-cap must say hidden tasks are hidden; got:\n%s", chunk)
	}
}

// ---------------------------------------------------------------------------
// Resize: SIGWINCH wakes the loop, and the frame picks up the new size.
// ---------------------------------------------------------------------------

func TestRunLoop_WakeTriggersImmediateRedraw(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, false)
	r.tty = true
	r.useColor = false
	r.setTerm(80, 24)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.RunLoop(ctx, time.Hour, nil, nil) // ticker never fires in a test
		close(done)
	}()

	r.Frame([]download.ProgressView{{ID: "a", Filename: "file-a", TotalSize: 100, BytesDone: 10, Speed: 1, ETA: time.Second, State: download.StateActive}}, nil)
	before := out.Len()

	// Wake → the loop re-renders from its last-seen state immediately.
	r.Wake <- struct{}{}
	select {
	case <-done:
		t.Fatal("RunLoop must not exit on wake (context not cancelled)")
	case <-time.After(50 * time.Millisecond):
	}
	if out.Len() <= before {
		t.Fatal("wake must trigger a redraw")
	}
}

// ---------------------------------------------------------------------------
// Non-TTY summary must stay a single fixed-width line.
// ---------------------------------------------------------------------------

func TestNonTTYSummary_UnchangedLayout(t *testing.T) {
	s := RenderSummary(1, 2, 44_000_000, 32*time.Second, 750<<20, 1<<30, false)
	if strings.Contains(s, "\n") {
		t.Fatalf("summary must be a single line: %q", s)
	}
	if !strings.Contains(s, "Total: 1/2 completed") {
		t.Fatalf("summary wrong: %s", s)
	}
}

// TestSummary_FitsAllWidths pins the tight-terminal fallback: when the full
// summary (with speed/ETA/bar) can't fit the terminal, it degrades to a short
// line that still shows the byte total + bar + percent — never truncating the
// pct off the right edge.
func TestRenderSummary_FitsAllWidths(t *testing.T) {
	for _, w := range []int{120, 80, 72, 60, 45, 30} {
		barW, _, _, _ := layoutFor(w)
		got := RenderSummaryWidth(0, 2, 44_000_000, 32*time.Second, 24<<20, 1<<30, false, w, barW)
		if d := displayWidth(got); d > w {
			t.Errorf("width %d: summary %d cells too wide: %q", w, d, got)
		}
		if !strings.Contains(got, "%") {
			t.Errorf("width %d: summary must keep the percent: %q", w, got)
		}
		if w == 120 && !strings.Contains(got, "ETA") {
			t.Errorf("width %d: full summary must keep ETA: %q", w, got)
		}
		if w <= 72 && strings.Contains(got, "ETA") {
			t.Errorf("width %d: compact summary must drop ETA: %q", w, got)
		}
	}
}
