// Package logging is ODM's tiny leveled logger (PRD §6.2 --log / --log-level).
// It is deliberately stdlib-shaped so we don't pull a logging framework:
// levels filter output, an optional --log file captures everything (mirrored),
// and the engine (Task/Manager) plus the CLI funnel here.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// Level is one of Debug/Info/Warn/Error.
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

// ParseLevel maps the --log-level string ("debug"/"info"/"warn"/"error").
func ParseLevel(s string) (Level, error) {
	switch s {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level %q", s)
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "?"
}

// Logger filters messages by level and writes to stderr (+ an optional mirror
// file when --log is set). It is safe for concurrent use.
type Logger struct {
	mu      sync.Mutex
	level   Level
	out     io.Writer // primary (stderr)
	file    *os.File  // optional mirror (the --log file); nil ⇒ none
	from    *log.Logger
	fileLog *log.Logger
}

// New builds a Logger at the given level. If logFile is non-empty, it is opened
// append-only and receives a copy of every emitted line.
func New(level Level, logFile string) (*Logger, error) {
	l := &Logger{
		level: level,
		out:   os.Stderr,
		from:  log.New(os.Stderr, "", log.LstdFlags),
	}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("--log %q: %w", logFile, err)
		}
		l.file = f
		l.fileLog = log.New(f, "", log.LstdFlags)
	}
	return l, nil
}

// SetOutput redirects the primary stream (default stderr). The CLI wires this
// to the UI renderer's frame-safe printer during an interactive run so engine
// logs can't corrupt the live progress frame; the --log file mirror is
// unaffected. Safe for concurrent use.
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.from.SetOutput(w)
}

// logf fans a formatted message to the primary writer (and the file mirror) if
// the level passes the threshold.
func (l *Logger) logf(lvl Level, format string, args ...any) {
	if l == nil || lvl > l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.from.Output(2, lvl.String()+": "+msg)
	if l.fileLog != nil {
		_ = l.fileLog.Output(2, lvl.String()+": "+msg)
	}
}

// Debugf / Infof / Warnf / Errorf are the level-tagged entry points.
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

// TaskLogFn adapts a Logger to the download.LogFn signature
// (level, format, args...) so the engine can plug straight in.
func (l *Logger) TaskLogFn() func(string, string, ...any) {
	if l == nil {
		return func(string, string, ...any) {}
	}
	return func(level string, format string, args ...any) {
		var lvl Level
		switch level {
		case "debug":
			lvl = LevelDebug
		case "warn":
			lvl = LevelWarn
		case "error":
			lvl = LevelError
		default:
			lvl = LevelInfo
		}
		l.logf(lvl, format, args...)
	}
}

// Close releases the mirror file handle, if any. Idempotent: a second Close
// is a no-op, matching the engine's defensive close-on-shutdown pattern.
func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}
