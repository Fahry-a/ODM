package ui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"odm/internal/download"
)

// syncBuf is a mutex-guarded bytes.Buffer (the production w is os.Stdout —
// writes to it are safe; the test needs a readable, race-clean buffer).

type raceBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *raceBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// TestRunLoop_InterjectWhileLive_RaceFree pins the M6 fix: engine logs are
// routed through InterjectWriter while RunLoop renders frames, and Interject
// reads r.cur.live/r.cur.queue via composeLines. RunLoop assigned those fields
// WITHOUT holding r.mu, so a log landing between the assignment and Frame
// raced with the reader. Under -race this test fails on unfixed code.
func TestRunLoop_InterjectWhileLive_RaceFree(t *testing.T) {
	out := &raceBuf{}
	r := NewRenderer(out, false)
	r.tty = true
	r.useColor = false
	r.setTerm(80, 24)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snap := make(chan []download.ProgressView, 1)
	qsnap := make(chan []download.ProgressView, 1)
	loopDone := make(chan struct{})
	go func() {
		r.RunLoop(ctx, time.Hour, snap, qsnap, loopDone) // ticker never fires
	}()

	view := download.ProgressView{ID: "a", Filename: "file-a", TotalSize: 100,
		BytesDone: 10, Speed: 1, ETA: time.Second, State: download.StateActive}
	snap <- []download.ProgressView{view}
	qsnap <- nil

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the logging goroutine (engine path via InterjectWriter)
		defer wg.Done()
		w := InterjectWriter(r)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = w.Write([]byte("engine: chunk retry warn line\n"))
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Add(1)
	go func() { // a second updater keeps the loop's assignments churning
		defer wg.Done()
		for i := range 200 {
			snap <- []download.ProgressView{{ID: "a", Filename: "file-a", TotalSize: 100,
				BytesDone: int64(10 + i), Speed: 1, ETA: time.Second, State: download.StateActive}}
			time.Sleep(time.Millisecond)
		}
	}()

	deadline := time.After(3 * time.Second)
	for i := 0; i < 200; i++ { // consume sends so nothing blocks
		select {
		case <-deadline:
			i = 200
		case <-time.After(2 * time.Millisecond):
		}
	}
	close(stop)
	cancel()
	<-loopDone
	wg.Wait()
}
