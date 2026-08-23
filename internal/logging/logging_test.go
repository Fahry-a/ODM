package logging

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseLevel verifies the --log-level string → Level mapping plus
// rejection of unknown values.
func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": LevelDebug,
		"info":  LevelInfo,
		"warn":  LevelWarn,
		"error": LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "trace", "verbose", "INFO"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("want error for %q", bad)
		}
	}
}

// TestLevel_String pins the textual prefix used in every log line so the
// engine/UI tests can match against it.
func TestLevel_String(t *testing.T) {
	cases := map[Level]string{
		LevelDebug: "DEBUG",
		LevelInfo:  "INFO",
		LevelWarn:  "WARN",
		LevelError: "ERROR",
	}
	for lvl, want := range cases {
		if got := lvl.String(); got != want {
			t.Errorf("level %d String = %q, want %q", lvl, got, want)
		}
	}
	// Unknown sentinel shouldn't crash — defensive for forward-compat.
	if unknown := Level(99).String(); unknown != "?" {
		t.Errorf("unknown level String = %q, want ?", unknown)
	}
}

// captureLogger builds a Logger whose primary sink is a buffer instead of
// os.Stderr — the production New() hard-wires os.Stderr, but since tests are
// in the same package we can swap the unexported `from` field after build.
func captureLogger(level Level) (*Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	l := &Logger{
		level: level,
		from:  log.New(&buf, "", 0), // no timestamp prefix → easier substring checks
	}
	return l, &buf
}

// contains reports whether `want` appears as a line tag in the captured log.
// The Output() path prepends "<LEVEL>: " so we look for that substring.
func contains(buf *bytes.Buffer, want string) bool {
	return strings.Contains(buf.String(), want)
}

// TestLogger_LevelFiltering walks the level ladder and asserts exactly the
// right methods make it through. The contract: a method passes when its level
// is <= the logger's threshold (`lvl > l.level` rejects), so higher Level
// value = more verbose. Error-only is the quietest, Debug the loudest.
func TestLogger_LevelFiltering(t *testing.T) {
	tests := []struct {
		level Level
		want  []string // expected emitted tags
		drop  []string // expected filtered out
	}{
		{LevelError, []string{"ERROR:"}, []string{"WARN:", "INFO:", "DEBUG:"}},
		{LevelWarn, []string{"ERROR:", "WARN:"}, []string{"INFO:", "DEBUG:"}},
		{LevelInfo, []string{"ERROR:", "WARN:", "INFO:"}, []string{"DEBUG:"}},
		{LevelDebug, []string{"ERROR:", "WARN:", "INFO:", "DEBUG:"}, nil},
	}
	for _, tt := range tests {
		l, buf := captureLogger(tt.level)
		l.Debugf("d=%d", 1)
		l.Infof("i=%d", 2)
		l.Warnf("w=%d", 3)
		l.Errorf("e=%d", 4)
		for _, w := range tt.want {
			if !contains(buf, w) {
				t.Errorf("level %d: missing expected tag %q in %q", tt.level, w, buf.String())
			}
		}
		for _, d := range tt.drop {
			if contains(buf, d) {
				t.Errorf("level %d: tag %q should have been filtered, in %q", tt.level, d, buf.String())
			}
		}
	}
}

// TestLogger_FormatPassthrough: args reach the underlying fmt.Sprintf, and
// the rendered message is concatenated with the level prefix.
func TestLogger_FormatPassthrough(t *testing.T) {
	l, buf := captureLogger(LevelInfo)
	l.Infof("hello %s #%d", "world", 42)
	out := buf.String()
	if !strings.Contains(out, "INFO: hello world #42") {
		t.Fatalf("unexpected output %q", out)
	}
}

// TestLogger_FileMirror: a --log file receives every emitted line in
// addition to the primary sink. We open the real file via New(), then swap
// the primary sink to io.Discard for the test's lifetime so it doesn't
// pollute test output (New hard-wires log to os.Stderr — same-package tests
// can rewrite that unexported field after construction).
func TestLogger_FileMirror(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "odm.log")
	l, err := New(LevelInfo, logPath)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	l.from = log.New(io.Discard, "", 0)
	t.Cleanup(func() { _ = l.Close() })

	l.Infof("alpha")
	l.Warnf("beta")
	l.Errorf("gamma")

	// Flush via Close so the file is fully written before we read it back.
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	body := string(b)
	for _, want := range []string{"INFO: alpha", "WARN: beta", "ERROR: gamma"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in file mirror; got:\n%s", want, body)
		}
	}
}

// TestLogger_New_BadPath: an un-openable --log path must surface as an error
// rather than silently swallowing the problem (otherwise the user thinks
// they're capturing logs when they aren't).
func TestLogger_New_BadPath(t *testing.T) {
	// /nonexistent/odm.log — directory doesn't exist → open fails.
	_, err := New(LevelInfo, "/nonexistent-dir/odm.log")
	if err == nil {
		t.Fatalf("want error for bad log path")
	}
	if !strings.Contains(err.Error(), "--log") {
		t.Fatalf("error should mention --log: %v", err)
	}
}

// TestLogger_New_WithoutFile: omitting logFile is the common case — Close
// must be a safe no-op.
func TestLogger_New_WithoutFile(t *testing.T) {
	l, err := New(LevelInfo, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if l.file != nil {
		t.Fatalf("file should be nil without --log")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestLogger_CloseIdempotent: defensive close — production code may call Close
// in both a defer and an explicit path on shutdown.
func TestLogger_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	l, err := New(LevelInfo, filepath.Join(dir, "x.log"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestLogger_NilSafe: a nil *Logger must never panic — the download engine
// sometimes passes nil to mean "engine runs silent",
// and a stray log call shouldn't crash it.
func TestLogger_NilSafe(t *testing.T) {
	var l *Logger
	if err := l.Close(); err != nil {
		t.Errorf("nil Close err = %v", err)
	}
	// All leveled methods on nil must be no-ops, not panics.
	l.Debugf("no")
	l.Infof("no")
	l.Warnf("no")
	l.Errorf("no")
}

// TestLogger_TaskLogFn_NilReturnsNoop: the engine's LogFn adapter on a nil
// logger must return a callable do-nothing — guards the download package
// against a nil deref when quiet+no-log collapse the logger to nil.
func TestLogger_TaskLogFn_NilReturnsNoop(t *testing.T) {
	var l *Logger
	fn := l.TaskLogFn()
	if fn == nil {
		t.Fatalf("TaskLogFn(nil) must return a non-nil no-op, not nil")
	}
	// Must not panic on any level string the engine emits.
	fn("debug", "x=%d", 1)
	fn("info", "x=%d", 2)
	fn("warn", "x=%d", 3)
	fn("error", "x=%d", 4)
	fn("nonsense", "ignored")
}

// TestLogger_TaskLogFn_LevelMapping: the engine passes string level names
// ("debug"/"info"/"warn"/"error"); TaskLogFn must route each to the right
// threshold so a LevelWarn logger does NOT see "debug" arrive.
func TestLogger_TaskLogFn_LevelMapping(t *testing.T) {
	l, buf := captureLogger(LevelWarn)
	fn := l.TaskLogFn()
	for _, lvl := range []string{"debug", "info", "warn", "error", "unknown"} {
		fn(lvl, "msg-%s", lvl)
	}
	out := buf.String()
	for _, keep := range []string{"WARN: msg-warn", "ERROR: msg-error"} {
		if !strings.Contains(out, keep) {
			t.Errorf("missing %q in %q", keep, out)
		}
	}
	for _, drop := range []string{"DEBUG: msg-debug", "INFO: msg-info"} {
		if strings.Contains(out, drop) {
			t.Errorf("level warn must filter %q; got %q", drop, out)
		}
	}
	// An unknown level string falls through to INFO per TaskLogFn's default
	// case — but at LevelWarn that INFO is also filtered, so it must not
	// appear in the buffer.
	if strings.Contains(out, "msg-unknown") {
		t.Errorf("unknown-level INFO must be filtered at LevelWarn; got %q", out)
	}
}
