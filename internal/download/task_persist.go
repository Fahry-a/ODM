package download

import (
	"time"

	"odm/internal/storage"
	"odm/internal/version"
)

// finish flushes and persist/removes the control file; an error from the
// caller already set the state.
func (t *Task) finish() error {
	if t.disk != nil {
		_ = t.disk.Sync()
	}
	// On success the control file is removed.
	if TaskState(t.state.Load()) == StateCompleted {
		_ = storage.RemoveControl(t.outPath)
	} else {
		// Persist what we have so a later --continue can resume.
		t.persistControl()
	}
	return nil
}

// effectiveChunkSize is the chunk size the current layout was built with:
// the aria2c segment size for that profile, else opts.ChunkSize. Persisted
// in the control file so a resume can rebuild the identical layout.
// effectiveChunkSize is the chunk size the current layout was built with:
// the aria2c segment size for that profile, else opts.ChunkSize. Persisted
// in the control file so a resume can rebuild the identical layout.
func (t *Task) effectiveChunkSize() int64 {
	if t.opts.Profile == "aria2c" && t.ariaSplit > 0 {
		pr := t.probe.Load()
		if pr != nil && pr.TotalSize > 0 {
			_, seg := AriaSplit(pr.TotalSize, int64(t.opts.Split), t.opts.MinSplitSize)
			return seg
		}
	}
	return t.opts.ChunkSize
}

// region2ChunkSize is the segment size of the both profile's second engine
// (0 for other profiles / legacy control files).
// region2ChunkSize is the segment size of the both profile's second engine
// (0 for other profiles / legacy control files).
func (t *Task) region2ChunkSize() int64 {
	if t.engines != nil {
		if pr := t.probe.Load(); pr != nil && pr.TotalSize > 0 {
			_, seg := AriaSplit(pr.TotalSize-t.splitAt, int64(t.opts.Split), t.opts.MinSplitSize)
			return seg
		}
	}
	return 0
}

// checkpoint returns true when the control file should be flushed: either
// persistCheckpointInterval chunks have completed since the last flush, or
// persistMinInterval has elapsed since the last write. Serialized under t.mu
// (the same lock persistControl takes) so the counters are race-free across
// concurrent workers.
// checkpoint returns true when the control file should be flushed: either
// persistCheckpointInterval chunks have completed since the last flush, or
// persistMinInterval has elapsed since the last write. Serialized under t.mu
// (the same lock persistControl takes) so the counters are race-free across
// concurrent workers.
func (t *Task) checkpoint() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chunksSincePersist++
	if t.chunksSincePersist >= persistCheckpointInterval {
		t.chunksSincePersist = 0
		return true
	}
	return !t.lastPersist.IsZero() && time.Since(t.lastPersist) >= persistMinInterval
}

// persistControl records completed chunk offsets so resume picks up. Mutex
// guarded: several workers (and the error/cancel paths) can call it
// concurrently, and SaveControl writes a shared temp path — concurrent writes
// would interleave and risk a corrupt .odm file.
// persistControl records completed chunk offsets so resume picks up. Mutex
// guarded: several workers (and the error/cancel paths) can call it
// concurrently, and SaveControl writes a shared temp path — concurrent writes
// would interleave and risk a corrupt .odm file.
func (t *Task) persistControl() {
	pr := t.probe.Load()
	if pr == nil || t.queue == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.controlCreatedAt.IsZero() {
		t.controlCreatedAt = now
	}
	t.lastPersist = now
	// Completed offsets are persisted as ABSOLUTE file offsets. For the both
	// profile the second engine's queue is 0-based within its region, so its
	// offsets are translated here (base + rel).
	var completed []int64
	if t.engines != nil {
		completed = append(completed, t.engines[0].CompletedOffsets()...)
		for _, off := range t.engines[1].CompletedOffsets() {
			completed = append(completed, off+t.engines[1].base)
		}
	} else {
		completed = t.queue.CompletedOffsets()
	}
	cf := &storage.ControlFile{
		URL:       t.url,
		FinalURL:  pr.FinalURL,
		TotalSize: pr.TotalSize,
		ChunkSize: t.effectiveChunkSize(),
		ETag:      pr.ETag,
		Completed: completed,
		// Per-chunk SHA-256 hashes for resume verification — only for chunks
		// recorded as completed (a hash can exist for a chunk whose bytes were
		// written but that never reached MarkDone; those must not be trusted).
		ChunkHashes: t.snapshotChunkHashes(completed),
		// Extended metadata
		CreatedAt:   t.controlCreatedAt,
		UpdatedAt:   now,
		Connections: int(t.conns.Load()),
		UserAgent:   t.opts.UserAgent,
		ODMVersion:  version.Version,
		Checksum:    t.opts.Checksum,
		// Profile metadata for layout reconstruction on resume.
		Profile:          t.opts.Profile,
		SplitAt:          t.splitAt,
		Region2ChunkSize: t.region2ChunkSize(),
	}
	if err := storage.SaveControl(t.outPath, cf); err != nil && !t.persistWarned.Swap(true) {
		// Best-effort by contract (a log failure must never fail the download),
		// but a full disk or permission error silently destroys resume state —
		// warn once per task so the user isn't surprised when --continue
		// doesn't pick up where it left off.
		t.logf("warn", "could not persist resume state %s: %v", storage.ControlPath(t.outPath), err)
	}
}

// unlimited. Used by RPC changeOption with "max-download-limit-per-task". Safe
// for concurrent use: creates a new limiter atomically (readers snapshot
// t.taskLim when wrapping the body, so an in-flight read finishes with the old
// value; subsequent reads pick up the new one).
