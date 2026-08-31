package ui

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"odm/internal/download"
)

// blockWriter simulates a block-buffered writer (e.g., stdio fully buffered
// when stdout is piped). Writes are held in an internal buffer and only become
// visible after Flush is called. Without an explicit Flush, data stays invisible
// until the buffer is full or a newline triggers a line-buffered flush — which
// is why the progress bar "only appears on Enter/new line" when the underlying
// writer is block-buffered and the renderer never flushes.
type blockWriter struct {
	pending bytes.Buffer // data written but not yet flushed
	visible bytes.Buffer // data that has been flushed (visible on screen)
}

func (w *blockWriter) Write(p []byte) (int, error) {
	return w.pending.Write(p)
}

func (w *blockWriter) Flush() error {
	_, err := w.visible.Write(w.pending.Bytes())
	w.pending.Reset()
	return err
}

// Sync is the *os.File path; blockWriter implements it via Flush so the
// renderer's os.File Sync() branch also makes data visible in tests.
func (w *blockWriter) Sync() error { return w.Flush() }

func (w *blockWriter) String() string { return w.visible.String() }

// TestProgress_FlushesWithoutNewline reproduces the bug where the progress bar
// is invisible until a newline (Enter) is pressed. With a block-buffered writer,
// Frame must explicitly flush after writing; otherwise the visible buffer stays
// empty until an external newline/Flush happens.
func TestProgress_FlushesWithoutNewline(t *testing.T) {
	bw := &blockWriter{}
	r := NewRenderer(bw, false)
	r.tty = true
	r.useColor = false
	r.sizeFn = func(io.Writer) (int, int) { return 120, 40 }

	view := []download.ProgressView{
		{ID: "t1", Filename: "tide-6.2.0.zip", State: download.StateActive, TotalSize: 84477, BytesDone: 37300, Connections: 1},
	}
	// First frame: should be visible immediately without needing an extra
	// newline/Enter from the user. Before the fix, visible was empty.
	r.Frame(view, nil)
	if got := bw.String(); !strings.Contains(got, "tide-6.2.0.zip") {
		t.Fatalf("progress bar must be flushed and visible after Frame without extra newline; got visible %q (pending %q)", got, bw.pending.String())
	}
	if !strings.Contains(bw.String(), "36") {
		t.Fatalf("progress bar must show bytes; got %q", bw.String())
	}
}

func TestProgress_Interject_Flushes(t *testing.T) {
	bw := &blockWriter{}
	r := NewRenderer(bw, false)
	r.tty = true
	r.useColor = false
	r.sizeFn = func(io.Writer) (int, int) { return 120, 40 }

	view := []download.ProgressView{
		{ID: "t1", Filename: "a.zip", State: download.StateActive, TotalSize: 100, BytesDone: 10, Connections: 1},
	}
	r.Frame(view, nil)
	_ = bw.Flush() // clear first frame's visible so we can check Interject alone
	bw.visible.Reset()
	bw.pending.Reset()

	r.Interject("engine: test log line\n")
	if got := bw.String(); !strings.Contains(got, "engine: test log line") {
		t.Fatalf("Interject must flush log line immediately; got %q", got)
	}
	if !strings.Contains(bw.String(), "a.zip") {
		t.Fatalf("Interject must redraw frame after log; got %q", bw.String())
	}
}

func TestProgress_BeginEnd_Flushes(t *testing.T) {
	bw := &blockWriter{}
	r := NewRenderer(bw, false)
	r.tty = true
	r.sizeFn = func(io.Writer) (int, int) { return 120, 40 }

	r.Begin()
	if got := bw.String(); !strings.Contains(got, ansiCursorHide) {
		t.Fatalf("Begin must flush cursor hide; got %q", got)
	}
	bw.visible.Reset()
	bw.pending.Reset()

	r.End()
	if got := bw.String(); !strings.Contains(got, ansiCursorShow) {
		t.Fatalf("End must flush cursor show; got %q", got)
	}
}
