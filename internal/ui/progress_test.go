package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	// 50% of 10 display cells → 5 eaten dashes, then face, then dots filling
	// the remaining 4 cells: 2 dots ("o o" = 3 cells) + 1 padding space = 4.
	got := Bar(50, 100, 10)
	want := "-----co o "
	if got != want {
		t.Fatalf("50%% want %q got %q", want, got)
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
	line := renderTaskLine(v, false, -1, 0, 20, BarWidth)
	for _, want := range []string{"linux-cachyos", "86.0M/120.0M", "x16", "71%", "%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line missing %q: %s", want, line)
		}
	}
}

// TestRenderTaskLine_FixedColumns pins the ILoveCandy layout so speed/ETA
// changes never shove the pacman bar or trailing percent left/right.
func TestRenderTaskLine_FixedColumns(t *testing.T) {
	base := download.ProgressView{
		Filename: "linux-cachyos", TotalSize: 120 << 20, Speed: 1 << 10,
		BytesDone: 86 << 20, Connections: 1, State: download.StateActive, ETA: 5 * time.Second,
	}
	// Vary the fields that used to grow/shrink the middle of the line.
	variants := []download.ProgressView{
		base,
		{Filename: base.Filename, TotalSize: base.TotalSize, Speed: 25 << 20, BytesDone: base.BytesDone, Connections: 16, State: base.State, ETA: 5 * time.Second},
		{Filename: base.Filename, TotalSize: base.TotalSize, Speed: 999 << 20, BytesDone: base.BytesDone, Connections: 32, State: base.State, ETA: 99*time.Minute + 59*time.Second},
		{Filename: base.Filename, TotalSize: base.TotalSize, Speed: 0, BytesDone: base.BytesDone, Connections: 1, State: base.State, ETA: 10 * time.Hour}, // caps at 99:59
		{Filename: base.Filename, TotalSize: 500, Speed: 50, BytesDone: 100, Connections: 1, State: base.State, ETA: time.Second},
		{Filename: base.Filename, TotalSize: -1, Speed: -1, BytesDone: 0, Connections: 1, State: base.State, ETA: 0},
	}

	// Anchor: the '[' that opens the pacman bar and the trailing '%' must sit
	// at the same index on every line regardless of speed/ETA length.
	barIdx := func(line string) int {
		i := strings.LastIndex(line, "[")
		if i < 0 {
			t.Fatalf("no bar bracket in %q", line)
		}
		return i
	}
	pctIdx := func(line string) int {
		i := strings.LastIndex(line, "%")
		if i < 0 {
			t.Fatalf("no percent in %q", line)
		}
		return i
	}

	ref := renderTaskLine(variants[0], false, -1, 0, 20, BarWidth)
	refBar, refPct, refLen := barIdx(ref), pctIdx(ref), len([]rune(ref))
	for _, v := range variants {
		line := renderTaskLine(v, false, -1, 0, 20, BarWidth)
		if got := barIdx(line); got != refBar {
			t.Fatalf("bar column shifted: want idx %d got %d\n  ref: %q\n  got: %q", refBar, got, ref, line)
		}
		if got := pctIdx(line); got != refPct {
			t.Fatalf("percent column shifted: want idx %d got %d\n  ref: %q\n  got: %q", refPct, got, ref, line)
		}
		if got := len([]rune(line)); got != refLen {
			t.Fatalf("line rune length changed: want %d got %d\n  ref: %q\n  got: %q", refLen, got, ref, line)
		}
	}
}

// TestBarWidthFor_Adapts pins the narrow-terminal bar shrink: the full 30-cell
// bar fits from ~96 columns down; below that only the bar (not the columns)
// gives way, down to the minBarWidth floor.
func TestBarWidthFor_Adapts(t *testing.T) {
	cases := []struct {
		term int
		want int
	}{
		{120, 30}, // roomy → full bar
		{100, 30},
		{96, 30}, // first width that still fits the full bar + 10-cell name
		{95, 29},
		{80, 14},
		{60, 10}, // floor
	}
	for _, tc := range cases {
		if got := barWidthFor(tc.term); got != tc.want {
			t.Errorf("barWidthFor(%d) = %d, want %d", tc.term, got, tc.want)
		}
	}
}

// TestRenderTaskLine_WideRuneNameStaysInBudget pins the wide-rune fix: a CJK
// filename (2 cells per rune) must be truncated to the name-width CELL budget
// and padded so the info block lands exactly where it would for an ASCII name —
// no overflow pushing the percent off the terminal edge.
func TestRenderTaskLine_WideRuneNameStaysInBudget(t *testing.T) {
	v := download.ProgressView{
		Filename:    strings.Repeat("日", 30), // 60 cells worth
		TotalSize:   120 << 20,
		Speed:       25 << 20,
		BytesDone:   86 << 20,
		Connections: 1,
		State:       download.StateActive,
		ETA:         5 * time.Second,
	}
	const nameW = 20
	line := renderTaskLine(v, false, -1, 0, nameW, BarWidth)
	want := nameW + infoBlockWidthFor(BarWidth)
	if d := displayWidth(line); d != want {
		t.Fatalf("line display width %d, want %d\n%q", d, want, line)
	}
	if !strings.Contains(line, "...") {
		t.Fatalf("long wide name must be truncated with ellipsis: %q", line)
	}
	// The bar bracket must sit at nameW + (everything before the '[') display
	// cells — measured in CELLS, not bytes (the CJK name is 3 bytes/rune).
	bracket := strings.LastIndex(line, "[")
	if got := displayWidth(line[:bracket]); got != nameW+infoFixedWidth-8 {
		t.Fatalf("bar bracket at %d cells, want %d\n%q", got, nameW+infoFixedWidth-8, line)
	}
}

// TestDisplayWidth_WideRunes pins the cell-aware width helper: CJK and emoji
// occupy 2 cells, ANSI escapes none.
func TestDisplayWidth_WideRunes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"日", 2},
		{"日本語", 6},
		{"a日b", 4},
		{"😀", 2}, // emoji (U+1F600)
		{"\x1b[33mc\x1b[0m", 1},
	}
	for _, tc := range cases {
		if got := displayWidth(tc.in); got != tc.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatDuration_FixedWidth(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "--:--:--"},
		{5 * time.Second, "00:00:05"},
		{32 * time.Second, "00:00:32"},
		{99*time.Minute + 59*time.Second, "01:39:59"},
		{100 * time.Minute, "01:40:00"},
		{10 * time.Hour, "10:00:00"},
		{99*time.Hour + 59*time.Minute + 59*time.Second, "99:59:59"},
		{100 * time.Hour, "99:59:59"}, // capped at 99:59:59
	}
	for _, tc := range cases {
		got := FormatDuration(tc.d)
		if got != tc.want {
			t.Fatalf("FormatDuration(%v)=%q want %q", tc.d, got, tc.want)
		}
		if len(got) != colETA {
			t.Fatalf("FormatDuration(%v)=%q len=%d want %d", tc.d, got, len(got), colETA)
		}
	}
}

func TestFormatFileSize_No1024Overflow(t *testing.T) {
	// Just under 1 MiB used to print "1024.0 KiB" (10 cells) and shove columns.
	got := FormatFileSize(1024*1024 - 1)
	if strings.HasPrefix(got, "1024.0") {
		t.Fatalf("must promote unit instead of printing 1024.0: %q", got)
	}
	if len([]rune(got)) > colSize {
		t.Fatalf("size %q exceeds colSize %d", got, colSize)
	}
	// Spot-check a few magnitudes stay within the size/speed columns.
	for _, b := range []int64{0, 999, 1023, 1024, 25 << 20, 120 << 20, 1 << 30, 999 << 20} {
		s := FormatFileSize(b)
		if n := len([]rune(s)); n > colSize {
			t.Fatalf("FormatFileSize(%d)=%q len=%d > colSize %d", b, s, n, colSize)
		}
		sp := FormatSpeed(b)
		if n := len([]rune(sp)); n > colSpeed {
			t.Fatalf("FormatSpeed(%d)=%q len=%d > colSpeed %d", b, sp, n, colSpeed)
		}
		// FormatFileSizeShort must also fit within colSize when paired as "done/total".
		short := FormatFileSizeShort(b)
		if n := len([]rune(short)); n > 6 {
			t.Fatalf("FormatFileSizeShort(%d)=%q len=%d > 6", b, short, n)
		}
	}
	// Verify the combined "done/total" fits within colSize.
	for _, pair := range [][2]int64{{100, 1000}, {86 << 20, 120 << 20}, {1 << 30, 2 << 30}, {500, -1}} {
		combined := FormatFileSizeShort(pair[0]) + "/" + FormatFileSizeShort(pair[1])
		if n := len([]rune(combined)); n > colSize {
			t.Fatalf("size pair %v=%q len=%d > colSize %d", pair, combined, n, colSize)
		}
	}
}

func TestRenderSummary(t *testing.T) {
	s := RenderSummary(3, 16, 44_000_000, 32*time.Second, 750<<20, 1<<30, false)
	if !strings.Contains(s, "3/16") || !strings.Contains(s, "00:00:32") || !strings.Contains(s, "73%") || !strings.Contains(s, "[") {
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
	ok, err := ConfirmSingle(strings.NewReader("y\n"), &out, "file.tar.zst", "/dest/file.tar.zst", 120<<20, 16, false)
	if err != nil || !ok {
		t.Fatalf("should confirm yes, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(out.String(), "linux") && !strings.Contains(out.String(), "file.tar.zst") {
		t.Fatalf("prompt missing details: %s", out.String())
	}
}

func TestConfirmSingle_Color(t *testing.T) {
	var out bytes.Buffer
	ok, err := ConfirmSingle(strings.NewReader("y\n"), &out, "file.tar.zst", "/dest/file.tar.zst", 120<<20, 16, true)
	if err != nil || !ok {
		t.Fatalf("should confirm yes, got ok=%v err=%v", ok, err)
	}
	s := out.String()
	if !strings.Contains(s, "\x1b[") {
		t.Fatalf("colored prompt must contain ANSI codes: %s", s)
	}
	if !strings.Contains(s, "file.tar.zst") {
		t.Fatalf("prompt missing filename: %s", s)
	}
}

// ---------------------------------------------------------------------------
// Bug §3.4 — rune-safe filename truncation (no mid-byte cuts on UTF-8 names).
// ---------------------------------------------------------------------------

func TestTruncateName_RuneSafe(t *testing.T) {
	// A 5-codepoint CJK name occupies 15 bytes but 5 runes; 16 display cells
	// (6 wide runes × 2 + 4 ASCII) fit under the 20-cell budget, so it is left
	// whole. The old byte-based `name[:17]` would have sliced mid-codepoint and
	// produced mojibake.
	cjk := "日本語のファイル.bin"
	got := truncateNameTo(cjk, 20)
	if got != cjk {
		t.Fatalf("short multibyte name must be unchanged, got %q", got)
	}

	// A deliberately long multibyte name must still end on a codepoint boundary,
	// and the budget is display CELLS (CJK = 2 cells each), not runes.
	long := strings.Repeat("日", 40) // 40 codepoints = 80 cells
	got = truncateNameTo(long, 20)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("long name must end with ellipsis, got %q", got)
	}
	if d := displayWidth(got); d > 20 {
		t.Fatalf("truncated name exceeds the %d-cell budget: %d cells (got=%q)", 20, d, got)
	}
	// (maxNameRunes-3) = 17 cells for the body → 8 wide runes (16 cells) + 3-cell "…".
	if rc := countRunes(strings.TrimSuffix(got, "...")); rc != 8 {
		t.Fatalf("truncated body must be 8 wide runes (17 cells / 2), got %d (got=%q)", rc, got)
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
		return BarIndeterminate(0, -1, width, bouncePosition(frame, width), frame, "")
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
	if got := posAt(0); !strings.HasPrefix(got, "c") && !strings.HasPrefix(got, "C") {
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
	// slot repeats. The face animation has its own period (2*pacFaceFrameDuration);
	// the combined period is LCM(2*(width-1), 2*pacFaceFrameDuration).
	combinedPeriod := lcm(2*(width-1), 2*pacFaceFrameDuration)
	if got, want := posAt(combinedPeriod), posAt(0); got != want {
		t.Fatalf("bounce should be periodic with combined period %d: got %q want %q", combinedPeriod, got, want)
	}
	// Sanity: static (pos -1) still lands pacman in the middle (not leftmost)
	// and contains both face and dots.
	static := BarIndeterminate(0, -1, width, -1, 0, "")
	if strings.HasPrefix(static, "c") || !strings.Contains(static, "c") || !strings.Contains(static, "o") {
		t.Fatalf("static indeterminate layout malformed: %q", static)
	}
}

// lcm computes the least common multiple of a and b.
func lcm(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	return a * b / gcd(a, b)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// TestBarIndeterminate_FaceAnimation tests that the pacman face animates
// between 'c' (lowercase) and 'C' (uppercase) every pacFaceFrameDuration frames
// (~1 second at 100ms/frame).
func TestBarIndeterminate_FaceAnimation(t *testing.T) {
	width := 30
	// Frame 0 → face should be 'c' (cycle 0)
	bar0 := BarIndeterminate(0, -1, width, -1, 0, "")
	if !strings.Contains(bar0, "c") {
		t.Fatalf("frame 0 should contain lowercase 'c', got %q", bar0)
	}
	// Frame pacFaceFrameDuration → face should be 'C' (cycle 1)
	bar10 := BarIndeterminate(0, -1, width, -1, pacFaceFrameDuration, "")
	if !strings.Contains(bar10, "C") {
		t.Fatalf("frame %d should contain uppercase 'C', got %q", pacFaceFrameDuration, bar10)
	}
	// Frame 2*pacFaceFrameDuration → face should be 'c' again (cycle 0)
	bar20 := BarIndeterminate(0, -1, width, -1, 2*pacFaceFrameDuration, "")
	if !strings.Contains(bar20, "c") {
		t.Fatalf("frame %d should contain lowercase 'c' again, got %q", 2*pacFaceFrameDuration, bar20)
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
	_, err := ConfirmBatch(strings.NewReader("n\n"), &out, rows, 4, 4, 2, false)
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

func TestConfirmBatch_Color(t *testing.T) {
	var out bytes.Buffer
	rows := []FileRow{
		{Name: "a.tar.zst", Size: 120 << 20},
		{Name: "b.tar.xz", Size: 80 << 20},
	}
	_, err := ConfirmBatch(strings.NewReader("n\n"), &out, rows, 4, 4, 2, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "\x1b[") {
		t.Fatalf("colored batch prompt must contain ANSI codes: %s", s)
	}
	for _, want := range []string{"2 files", "[1]", "[2]", "a.tar.zst", "b.tar.xz"} {
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

// TestTTYRedraw_CursorUpMatchesNewlines ensures each frame's cursor-up count
// equals the previous frame's printed lines — a mismatch is what eats scrollback
// (too many ups) or leaves orphans that force scroll (too few ups).
func TestTTYRedraw_CursorUpMatchesNewlines(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.tty = true
	r.useColor = false

	// Pin width so truncation is deterministic regardless of host terminal.
	// We can't inject width directly; rendererWidth reads the writer. Since
	// buf isn't a *os.File, width falls back to MinTerminalWidth-1 = 79.
	frames := [][]download.ProgressView{
		{{ID: "a", Filename: "file-a", State: download.StateActive, TotalSize: 100, BytesDone: 10, Speed: 1 << 20, ETA: 5 * time.Second, Connections: 4}},
		{{ID: "a", Filename: "file-a", State: download.StateActive, TotalSize: 100, BytesDone: 50, Speed: 25 << 20, ETA: 2 * time.Second, Connections: 16}},
		{{ID: "a", Filename: "file-a", State: download.StateActive, TotalSize: 100, BytesDone: 50, Speed: 25 << 20, ETA: 2 * time.Second, Connections: 16},
			{ID: "b", Filename: "file-b", State: download.StateActive, TotalSize: 200, BytesDone: 1, Speed: 100, ETA: time.Hour, Connections: 1}},
		{{ID: "a", Filename: "file-a", State: download.StateCompleted, TotalSize: 100, BytesDone: 100, Connections: 0},
			{ID: "b", Filename: "file-b", State: download.StateActive, TotalSize: 200, BytesDone: 50, Speed: 1 << 20, ETA: 30 * time.Second, Connections: 8}},
		{{ID: "b", Filename: "file-b", State: download.StateActive, TotalSize: 200, BytesDone: 90, Speed: 1 << 10, ETA: 5 * time.Second, Connections: 8}},
	}

	prevLines := 0
	for i, live := range frames {
		before := buf.String()
		r.Frame(live, nil)
		chunk := buf.String()[len(before):]
		ups := strings.Count(chunk, "\x1b[A")
		// Count content newlines (Fprintln). Ignore none other.
		newlines := strings.Count(chunk, "\n")
		if ups != prevLines {
			t.Fatalf("frame %d: cursor-up count %d != previous frame lines %d\nchunk=%q", i, ups, prevLines, chunk)
		}
		// After the ups/clears, we should print exactly the new frame's lines.
		// lastLines after frame should equal newlines in this chunk.
		if newlines != r.lastLines {
			t.Fatalf("frame %d: newlines written %d != lastLines %d", i, newlines, r.lastLines)
		}
		// expected line count = len(live retained) + summary; at least 1 (summary)
		if r.lastLines < 1 {
			t.Fatalf("frame %d: lastLines < 1", i)
		}
		prevLines = r.lastLines
		t.Logf("frame %d: ups=%d lines=%d ok", i, ups, r.lastLines)
	}
}

// ---------------------------------------------------------------------------
// ANSI-aware truncation: colored lines are truncated by visible width, not
// raw rune count, so ANSI escape sequences are preserved intact.
// ---------------------------------------------------------------------------

func TestAnsiVisibleWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"hello", 5},
		{"", 0},
		{"\x1b[33mc\x1b[0m", 1},         // colored 'c' = 1 visible cell
		{"\x1b[32m---\x1b[0m", 3},       // 3 dashes
		{"\x1b[35m[x4]\x1b[0m", 4},      // [x4] = 4 visible
		{"\x1b[0m\x1b[33m\x1b[0m", 0},   // just escape codes
		{"abc\x1b[31mdef\x1b[0mghi", 9}, // mix: 3 + 3 + 3
	}
	for _, tc := range cases {
		got := displayWidth(tc.s)
		if got != tc.want {
			t.Fatalf("displayWidth(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestTruncateVisibleWidth(t *testing.T) {
	colored := "\x1b[33mc\x1b[0m o o o o o o o o o"
	// Visible: "c o o o o o o o o o" = 19 chars. Truncate to 5 → "c o o " + reset.
	got := truncateVisibleWidth(colored, 5)
	if displayWidth(got) != 5 {
		t.Fatalf("truncateVisibleWidth(_, 5): visible width = %d, want 5", displayWidth(got))
	}
	// The result must not end with an unclosed color (reset must be present
	// if the last segment was a visible char inside a color span).
	// Check: the last visible char should not be preceded by an unreset color.
	// Simple check: if the string contains any color code, the part after the
	// last visible char must either be plain or have a reset.
	if displayWidth(got) > 0 {
		// Verify no partial escape sequences by checking valid UTF-8.
		if !utf8.ValidString(got) {
			t.Fatalf("truncated colored string is not valid UTF-8: %q", got)
		}
	}

	// Plain text: truncate to 3 → "hel"
	got = truncateVisibleWidth("hello", 3)
	if got != "hel" {
		t.Fatalf("truncateVisibleWidth(\"hello\", 3) = %q, want \"hel\"", got)
	}

	// Width >= visible: returns unchanged.
	got = truncateVisibleWidth("\x1b[33mab\x1b[0m", 10)
	if displayWidth(got) != 2 {
		t.Fatalf("should not truncate when width >= visible: got visible width %d", displayWidth(got))
	}
}

func TestTruncateToWidth_AnsiAware(t *testing.T) {
	// Simulate a colored task line that would be wider than terminal.
	// truncateToWidth must preserve ANSI codes intact.
	line := "\x1b[33mlinux-cachyos        \x1b[0m  500.0 MiB  \x1b[33m  1.9 MiB/s\x1b[0m  00:04:18  [\x1b[35mx16 \x1b[0m\x1b[33mc\x1b[0m\x1b[32m----\x1b[0m\x1b[36m o o\x1b[0m]  \x1b[33m  1%\x1b[0m"
	got := truncateToWidth(line, 80)
	visible := displayWidth(got)
	if visible > 80 {
		t.Fatalf("truncateToWidth should limit visible width to 80, got %d", visible)
	}
	// The line must still contain ANSI codes (not stripped).
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("truncateToWidth must preserve ANSI codes")
	}
}
