package rpc

import (
	"sync"
	"time"

	"odm/internal/download"
)

// ProgressThrottler coalesces the scheduler's high-frequency progress snapshots
// into coarse WebSocket events so a large batch doesn't flood subscribers. It
// forwards one onDownloadProgress event per live task at most once per interval;
// snapshots between ticks are dropped (the next tick re-reads fresh state from
// the engine, so no progress is lost — only intermediate ones).
type ProgressThrottler struct {
	bc   *Broadcaster
	mu   sync.Mutex
	last time.Time
}

// NewProgressThrottler wraps bc with a ~250ms emission floor.
func NewProgressThrottler(bc *Broadcaster) *ProgressThrottler {
	return &ProgressThrottler{bc: bc}
}

// Forward is the scheduler.ProgressCB wired into NewEmptyScheduler. It emits
// onDownloadProgress for every live task at most once per tick; queued tasks
// are not forwarded (they have no live bytes yet) to keep the feed meaningful.
func (p *ProgressThrottler) Forward(live, _ []download.ProgressView) {
	p.mu.Lock()
	if time.Since(p.last) < 250*time.Millisecond {
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	for _, v := range live {
		p.bc.Broadcast(Event{
			Method: "onDownloadProgress",
			Params: SnapshotParams(v),
		})
	}
}
