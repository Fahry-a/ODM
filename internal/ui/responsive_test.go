package ui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
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
		{60, 10, layoutNameBarPct}, // bar at floor, no size/speed/ETA
		{45, 10, layoutNameBarPct},
		{40, 10, layoutNameBarPct},
		{36, 10, layoutNameBarPct}, // name+bar+pct floor
		{35, 0, layoutNamePct},     // barless: name + pct
		{20, 0, layoutNamePct},
		{12, 0, layoutNamePct}, // name+pct floor
		{10, 0, layoutPct},     // super narrow: pct only
		{8, 0, layoutPct},
		{4, 0, layoutPct},
	}
	for _, tc := range cases {
		barW, nameW, minW, layout := layoutFor(tc.term)
		if barW != tc.barW || layout != tc.layout {
			t.Errorf("layoutFor(%d) = barW %d layout %v, want barW %d layout %v",
				tc.term, barW, layout, tc.barW, tc.layout)
		}
		if minW < 0 || minW > tc.term {
			t.Errorf("layoutFor(%d): minW %d must fit in %d cols", tc.term, minW, tc.term)
		}
		if tc.layout == layoutFull && nameW < 8 {
			t.Errorf("layoutFor(%d): full layout leaves only %d cells for the name", tc.term, nameW)
		}
	}
}

// TestRenderTaskLine_SuperSmall pins the ultra-narrow tiers: every width from
// 120 down to 4 columns renders a non-wrapping line that fits.
func TestRenderTaskLine_SuperSmall(t *testing.T) {
	v := download.ProgressView{
		Filename: "linux-cachyos-6.9.1-x86_64.iso", TotalSize: 2 << 30, Speed: 50 << 20,
		BytesDone: 1 << 30, Connections: 16, State: download.StateActive, ETA: 30 * time.Second,
	}
	for _, width := range []int{120, 80, 66, 60, 45, 40, 30, 25, 20, 15, 12, 11, 10, 8, 5, 4} {
		barW, nameW, _, layout := layoutFor(width)
		line := renderTaskLine(v, false, -1, nameW, barW, layout)
		if d := displayWidth(line); d > width {
			t.Errorf("width %d: line %d cells too wide: %q", width, d, line)
		}
		if layout == layoutPct {
			if strings.Contains(line, "linux") {
				t.Errorf("width %d: pct-only line must not show the name: %q", width, line)
			}
		} else if layout == layoutNamePct && strings.Contains(line, "[") {
			t.Errorf("width %d: barless layout must have no bracket: %q", width, line)
		}
		if !strings.Contains(line, "%") && layout != layoutPct {
			t.Errorf("width %d: line missing percent: %q", width, line)
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
		line := renderTaskLine(v, false, -1, nameW, barW, layout)
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
	line := renderTaskLine(v, true, -1, 6, 0, layoutNamePct)
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

// syncBuf is a mutex-guarded bytes.Buffer so a test can read Len() while the
// RunLoop goroutine writes to it — bytes.Buffer is not goroutine-safe, and the
// original test raced a direct r.Frame call from the test goroutine against
// the loop's own Frame/Begin writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// waitGrow blocks (bounded) until out's length exceeds prev and returns the
// new length — used to wait for the RunLoop goroutine to finish rendering a
// frame queued to it before the test reads the buffer.
func waitGrow(out *syncBuf, prev int) int {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if out.Len() > prev {
			return out.Len()
		}
		time.Sleep(5 * time.Millisecond)
	}
	return out.Len()
}

func TestRunLoop_WakeTriggersImmediateRedraw(t *testing.T) {
	out := &syncBuf{}
	r := NewRenderer(out, false)
	r.tty = true
	r.useColor = false
	r.setTerm(80, 24)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snap := make(chan []download.ProgressView, 1)
	loopDone := make(chan struct{})
	go func() {
		r.RunLoop(ctx, time.Hour, snap, nil, loopDone) // ticker never fires in a test
	}()

	// Feed the initial view through the loop, not a direct r.Frame call — the
	// loop owns the output buffer, so the test goroutine must not write to it.
	snap <- []download.ProgressView{{ID: "a", Filename: "file-a", TotalSize: 100, BytesDone: 10, Speed: 1, ETA: time.Second, State: download.StateActive}}
	before := waitGrow(out, 0)

	// Wake → the loop re-renders from its last-seen state immediately.
	r.Wake <- struct{}{}
	select {
	case <-loopDone:
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
	s := RenderSummary(1, 2, 44_000_000, 32*time.Second, 0, 750<<20, 1<<30, false)
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
	for _, w := range []int{120, 80, 72, 60, 45, 30, 20, 10, 4} {
		barW, _, _, _ := layoutFor(w)
		got := RenderSummaryWidth(0, 2, 44_000_000, 32*time.Second, 0, 24<<20, 1<<30, false, w, barW)
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
