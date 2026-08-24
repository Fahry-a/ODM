package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"odm/internal/download"
)

// TestRun_TallyIncludesLastCompletions pins the doneCh drain: launch posts a
// task's completion to s.compl AFTER wg.Done, so when the last two tasks
// finish back-to-back, Run's select can pick doneCh while the final report is
// still sitting in compl's buffer. Without the non-blocking drain before the
// return, that task is lost from the succeeded/failed tally (and from exit
// codes and RPC completion events). Runs repeatedly — the race needs both
// completions to land within the select's scheduling window.
func TestRun_TallyIncludesLastCompletions(t *testing.T) {
	for iter := range 30 {
		mgr, err := download.NewManager(download.ExecOptions{
			Dir:         t.TempDir(),
			Connections: 1,
			ChunkSize:   4096,
			Retry:       0,
			Timeout:     10 * time.Second,
			CheckCert:   true,
		}, nil)
		if err != nil {
			t.Fatalf("iter %d: NewManager: %v", iter, err)
		}

		var (
			mu        sync.Mutex
			completed int
		)
		sch := NewScheduler(&Plan{
			Parallel: []Allocation{
				{URL: "http://example.invalid/a.bin", Connections: 1},
				{URL: "http://example.invalid/b.bin", Connections: 1},
			},
		}, mgr.NewTask, nil)
		sch.OnComplete(func(download.ProgressView) {
			mu.Lock()
			completed++
			n := completed
			mu.Unlock()
			if n > 2 {
				t.Errorf("iter %d: more than 2 completion events", iter)
			}
		})

		got := make(chan int, 1)
		go func() {
			ok, failed, _ := sch.Run(context.Background())
			got <- ok + failed
		}()

		select {
		case total := <-got:
			if total != 2 {
				t.Fatalf("iter %d: Run tallied %d of 2 finished tasks", iter, total)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("iter %d: Run did not return", iter)
		}
		// Run has returned; handleComplete runs on Run's own goroutine before
		// the return, so the counter is stable now.
		mu.Lock()
		n := completed
		mu.Unlock()
		if n != 2 {
			t.Fatalf("iter %d: %d of 2 completion events fired", iter, n)
		}
	}
}
