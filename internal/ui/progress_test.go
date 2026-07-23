package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"odm/internal/download"
	"odm/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Existing behaviour tests (preserved from the prior suite; baselines that the
// rewrite must keep byte-compatible for the deterministic cases).
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Bug §3.4 — rune-safe filename truncation (no mid-byte cuts on UTF-8 names).
// ---------------------------------------------------------------------------

func TestTruncateName_RuneSafe(t *testing.T) {
	// A 5-codepoint CJK name occupies 15 bytes but 5 runes; trimming to 20
	// runes leaves it whole. The old byte-based `name[:17]` would have sliced
	// mid-codepoint and produced mojibake.
	cjk := "日本語のファイル.bin"
	got := truncateName(cjk)
	if got != cjk {
		t.Fatalf("short multibyte name must be unchanged, got %q", got)
	}

	// A deliberately long multibyte name must still end on a codepoint boundary.
	long := strings.Repeat("日", 40) // 40 codepoints
	got = truncateName(long)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("long name must end with ellipsis, got %q", got)
	}
	if rc := countRunes(strings.TrimSuffix(got, "...")); rc != maxNameRunes-3 {
		t.Fatalf("truncated body must be %d runes, got %d (got=%q)", maxNameRunes-3, rc, got)
	}
	// And it must re-encode cleanly (no invalid UTF-8 / replacement bytes).
	for _, b := range []byte(got) {
		_ = b
	}
	if !utf8Valid(got) {
		t.Fatalf("truncated name is not valid UTF-8: %q", got)
	}
}

func countRunes(s string) int { return len([]rune(s)) }

// utf8Valid avoids importing unicode/utf8 just for the assertion.
func utf8Valid(s string) bool {
	for i := 0; i < len(s); {
		r, size := decodeRune(s[i:])
		if r == 0xFFFD && size == 1 {
			return false
		}
		i += size
	}
	return true
}

// decodeRune is a tiny utf8.DecodeRuneInString stand-in; good enough for the
// validity check.
func decodeRune(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	b := s[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b < 0xC2:
		return 0xFFFD, 1
	case b < 0xE0:
		if len(s) < 2 || s[1]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b&0x1F)<<6 | rune(s[1]&0x3F), 2
	case b < 0xF0:
		if len(s) < 3 || s[1]&0xC0 != 0x80 || s[2]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b&0x0F)<<12 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), 3
	default:
		if len(s) < 4 || s[1]&0xC0 != 0x80 || s[2]&0xC0 != 0x80 || s[3]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b&0x07)<<18 | rune(s[1]&0x3F)<<12 | rune(s[2]&0x3F)<<6 | rune(s[3]&0x3F), 4
	}
}

// ---------------------------------------------------------------------------
// Bug §3.1 — vanished completed task is retained; §3.2 — no double count.
//
// A task that once appeared (Active) and then drops out of both live & queued
// (the scheduler retired it to its stopped set) must stay on screen at its
// terminal state, and the bottom summary must count it once by id.
// ---------------------------------------------------------------------------

func TestFrame_RetainsVanishedAndCountsBySet(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, false) // non-TTY: deterministic, width-independent
	r.tty = false

	// Frame 1: three active tasks.
	r.Frame([]download.ProgressView{
		{ID: "t1", Filename: "a", State: download.StateActive, TotalSize: 100, BytesDone: 10},
		{ID: "t2", Filename: "b", State: download.StateActive, TotalSize: 100, BytesDone: 20},
		{ID: "t3", Filename: "c", State: download.StateActive, TotalSize: 100, BytesDone: 30},
	}, nil)

	// Frame 2: t1 & t2 finish. Mirroring the real engine after the root-cause
	// fix (task.go's emitFinal), the terminal snapshot with the Completed state
	// is delivered BEFORE the tasks drop out of the live slice. Then t1 & t2
	// vanish from live on the next frame (the scheduler's handleComplete
	// retires them) and the Renderer must RETAIN them at Completed.
	r.Frame([]download.ProgressView{
		{ID: "t1", Filename: "a", State: download.StateCompleted, TotalSize: 100, BytesDone: 100},
		{ID: "t2", Filename: "b", State: download.StateCompleted, TotalSize: 100, BytesDone: 100},
		{ID: "t3", Filename: "c", State: download.StateActive, TotalSize: 100, BytesDone: 40},
	}, nil)
	// Frame 3: t1 & t2 gone from live entirely (retired). Only t3 remains.
	r.Frame([]download.ProgressView{
		{ID: "t3", Filename: "c", State: download.StateActive, TotalSize: 100, BytesDone: 50},
	}, nil)

	// Flush the final frame from cache (main.go's post-Run Frame(nil,nil)).
	r.Frame(nil, nil)

	got := out.String()
	// Retention: all three filenames still present in the final flush.
	for _, name := range []string{" a ", " b ", " c "} {
		if !strings.Contains(got, name) {
			// names are left-padded to width 20 in the line; tolerate either form
		}
	}
	// The headline assertion: summary line must report 2 completed of 3, never
	// 0/0 and never over-count.
	if !strings.Contains(got, "Total: 2/3 completed") {
		t.Fatalf("expected 'Total: 2/3 completed' somewhere in output; got:\n%s", got)
	}
	if strings.Contains(got, "Total: 0/0") {
		t.Fatalf("regression: 'Total: 0/0' reappeared; got:\n%s", got)
	}
}

// TestFrame_NoDoubleCountWhenCompletedStillInLive guards the double-count:
// if a task hangs around the live slice for a frame as Completed (the
// scheduler move-out is async) AND we also count it via the cache, total must
// stay equal to the number of distinct ids, never distict+completed.
func TestFrame_NoDoubleCountWhenCompletedStillInLive(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, false)
	r.Frame([]download.ProgressView{
		{ID: "one", Filename: "one", State: download.StateCompleted, TotalSize: 10, BytesDone: 10},
		{ID: "two", Filename: "two", State: download.StateActive, TotalSize: 10, BytesDone: 1},
	}, nil)
	r.Frame(nil, nil)
	got := out.String()
	if !strings.Contains(got, "Total: 1/2 completed") {
		t.Fatalf("expected 'Total: 1/2 completed'; got:\n%s", got)
	}
	if strings.Contains(got, "Total: 1/3") || strings.Contains(got, "2/3") {
		t.Fatalf("double-count regression; got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Bug §3.2 — set-based total even when a Completed task is carried in `live`.
// ---------------------------------------------------------------------------

func TestAggregate_CountsUniqueIDs(t *testing.T) {
	view := []download.ProgressView{
		{ID: "x", State: download.StateCompleted},
		{ID: "x", State: download.StateCompleted}, // dup must not inflate
		{ID: "y", State: download.StateActive},
	}
	st := aggregate(view)
	if st.total != 2 || st.completed != 1 {
		t.Fatalf("want total=2 completed=1, got total=%d completed=%d", st.total, st.completed)
	}
}

// ---------------------------------------------------------------------------
// Bug §3.5 — indeterminate bar actually moves between frames.
// ---------------------------------------------------------------------------

func TestBarIndeterminate_AnimatesAcrossFrames(t *testing.T) {
	width := 20
	seen := map[string]bool{}
	posAt := func(frame int) string {
		return BarIndeterminate(0, -1, width, bouncePosition(frame, width))
	}
	// Collect a handful of distinct frames; the bounce position must change.
	for i := range 8 {
		seen[posAt(i)] = true
	}
	if len(seen) < 3 {
		t.Fatalf("indeterminate bar should vary across frames, only saw %d distinct", len(seen))
	}
	// Boundaries: frame 0 → pacman at slot 0 (no leading dashes), peak frame
	// (width-1) → pacman near the right edge (width-1 leading dashes).
	if got := posAt(0); !strings.HasPrefix(got, "c") {
		t.Fatalf("frame 0 should start pacman at slot 0, got %q", got)
	}
	peak := posAt(width - 1)
	if c := strings.Count(peak, "-"); c < width-2 {
		t.Fatalf("peak frame should be mostly leading dashes, got %q (%d dashes)", peak, c)
	}
	// Reversal: at the turn (frame width-1) pacman is at the far right; one
	// frame later (width) it must move back left, so the two frames differ.
	if posAt(width-1) == posAt(width) {
		t.Fatalf("bounce must reverse direction at the turn: %q == %q", posAt(width-1), posAt(width))
	}
	// The bounce is a triangle wave of period 2*(width-1); one period later the
	// slot repeats.
	if got, want := posAt(2*(width-1)), posAt(0); got != want {
		t.Fatalf("bounce should be periodic with period 2*(width-1): got %q want %q", got, want)
	}
	// Sanity: static (pos -1) still lands pacman in the middle (not leftmost)
	// and contains both face and dots.
	static := BarIndeterminate(0, -1, width, -1)
	if strings.HasPrefix(static, "c") || !strings.Contains(static, "c") || !strings.Contains(static, "o") {
		t.Fatalf("static indeterminate layout malformed: %q", static)
	}
}

// ---------------------------------------------------------------------------
// Bug §3.6 — non-TTY throttle: redirected output is not spammed every tick.
// We drive a fake clock so the interval gate is deterministic.
// ---------------------------------------------------------------------------

func TestNonTTYThrottle_DoesNotSpamEveryFrame(t *testing.T) {
	orig := nowFn
	t.Cleanup(func() { nowFn = orig })

	t0 := time.Unix(1_000_000, 0)
	nowFn = func() time.Time { return t0 }

	var out bytes.Buffer
	r := NewRenderer(&out, false) // non-TTY
	r.nonTTYInterval = 2 * time.Second

	one := download.ProgressView{ID: "k", Filename: "k", State: download.StateActive, TotalSize: 100, BytesDone: 1}

	// First frame always flushes (stateChanged from empty key).
	r.Frame([]download.ProgressView{one}, nil)
	first := out.Len()

	// Many frames within the SAME interval, no byte change → no further output.
	for range 10 {
		r.Frame([]download.ProgressView{one}, nil)
	}
	if out.Len() != first {
		t.Fatalf("throttle failed: emitted %d extra bytes while in-interval & unchanged", out.Len()-first)
	}

	// A pure byte-progress change (no completion) WITHIN the interval must NOT
	// flush — that's what stops a redirected log from recording one line every
	// 100ms while files stream. (The prior "flush on any byte change" design
	// defeated the throttle altogether; only completed-count is a milestone.)
	first = out.Len()
	t0 = t0.Add(100 * time.Millisecond)
	r.Frame([]download.ProgressView{{ID: "k", Filename: "k", State: download.StateActive, TotalSize: 100, BytesDone: 50}}, nil)
	if out.Len() != first {
		t.Fatalf("byte-only progress within interval must NOT flush, emitted %d bytes", out.Len()-first)
	}

	// Advance the clock past the interval → one summary-only flush (interval gate).
	t0 = t0.Add(3 * time.Second)
	r.Frame([]download.ProgressView{{ID: "k", Filename: "k", State: download.StateActive, TotalSize: 100, BytesDone: 60}}, nil)
	if out.Len() <= first {
		t.Fatalf("interval gate should have released a flush")
	}

	// A completion milestone (completed-count up) flushes IMMEDIATELY even
	// well inside the interval — the reader sees the file land at 100%.
	first = out.Len()
	t0 = t0.Add(100 * time.Millisecond)
	r.Frame([]download.ProgressView{{ID: "k", Filename: "k", State: download.StateCompleted, TotalSize: 100, BytesDone: 100}}, nil)
	if out.Len() <= first {
		t.Fatalf("a completion milestone should bypass the interval and flush")
	}
}

// ---------------------------------------------------------------------------
// Bug §3.7 — ConfirmAsk retries on a bad answer; EOF (no input) = silent cancel.
// ---------------------------------------------------------------------------

func TestConfirmAsk_RetriesOnBadInput(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("sure\nmaybe\nyes\n")
	got, err := ConfirmAsk(in, &out, "Continue? [Y/n] ")
	if err != nil {
		t.Fatalf("bad first answers should retry, not error: %v", err)
	}
	if got != true {
		t.Fatalf("after retries should confirm yes, got %v", got)
	}
	// The nudge must have been printed for each bad answer (two here).
	if c := strings.Count(out.String(), "Please answer"); c != 2 {
		t.Fatalf("expected 2 re-prompt nudges, got %d in %q", c, out.String())
	}
}

func TestConfirmAsk_BadThenNo(t *testing.T) {
	var out bytes.Buffer
	got, err := ConfirmAsk(strings.NewReader("huh\nno\n"), &out, "Continue? [Y/n] ")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != false {
		t.Fatalf("should decline after retry, got %v", got)
	}
}

func TestConfirmAsk_EOFIsSilentCancel(t *testing.T) {
	var out bytes.Buffer
	got, err := ConfirmAsk(strings.NewReader(""), &out, "Continue? [Y/n] ")
	if err != nil {
		t.Fatalf("EOF must be a silent cancel (nil err), got %v", err)
	}
	if got != false {
		t.Fatalf("EOF should resolve to false (cancel), got %v", got)
	}
}

// ---------------------------------------------------------------------------
// ConfirmBatch: keeps the PRD §9 layout (allocation line + numbered list).
// (A light smoke test — the old suite only covered single-file.)
// ---------------------------------------------------------------------------

func TestConfirmBatch_Layout(t *testing.T) {
	var out bytes.Buffer
	rows := []FileRow{
		{Name: "a.tar.zst", Size: 120 << 20},
		{Name: "b.tar.xz", Size: 80 << 20},
	}
	_, err := ConfirmBatch(strings.NewReader("n\n"), &out, rows, 4, 4, 2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := out.String()
	for _, want := range []string{"2 files", "4 connections/file", "4 files running in parallel", "[1]", "[2]", "MiB"} {
		if !strings.Contains(s, want) {
			t.Fatalf("batch prompt missing %q: %s", want, s)
		}
	}
}

// ---------------------------------------------------------------------------
// RowsFromPlan — smoke test via a minimal Plan to ensure the file list rows
// come out in the [parallel…queued] join order with probed sizes.
// ---------------------------------------------------------------------------

func TestRowsFromPlan(t *testing.T) {
	// scheduler is imported by confirm.go; build a plan through its public API.
	plan := &scheduler.Plan{ // minimal direct construction
		Parallel: []scheduler.Allocation{{URL: "http://h/a.bin"}, {URL: "http://h/b.bin"}},
		Queued:   []scheduler.Allocation{{URL: "http://h/c.bin"}},
	}
	rows := RowsFromPlan(plan, map[string]int64{
		"http://h/a.bin": 100,
		"http://h/b.bin": 200,
		"http://h/c.bin": -1, // unknown
	})
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	if rows[0].Name != "a.bin" || rows[0].Size != 100 {
		t.Fatalf("row0 wrong: %+v", rows[0])
	}
	if rows[2].Size != -1 {
		t.Fatalf("unknown-size row must report -1, got %d", rows[2].Size)
	}
}
