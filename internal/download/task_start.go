package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"odm/internal/storage"
)

// Start runs Probe → open file → start workers. Blocks until the task finishes
// (completed or errored) or ctx is cancelled. progressSink receives periodic
// snapshots for the UI/RPC aggregator; pass nil to opt out.
func (t *Task) Start(ctx context.Context, conns int, progressSink func(ProgressView)) error {
	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	defer cancel()
	// A task removed via RPC while still queued never reached Start before;
	// Cancel had no ctx to cancel. If it was cancelled, fail fast instead of
	// downloading a file the caller no longer wants.
	if t.cancelled.Load() {
		cancel()
		t.setState(StateError)
		t.emitFinal(progressSink)
		return fmt.Errorf("task cancelled before start")
	}
	t.setState(StateActive)
	t.baseCtx = ctx
	t.sink = progressSink

	// 1. Probe. The CLI one-shot path already probed every URL for the Balancer
	// and confirmation prompt and injects the result via SetProbe, so a fresh
	// network probe (HEAD + ranged GET) is skipped there. The RPC daemon path
	// leaves t.probe nil and probes here as usual.
	pr := t.probe.Load()
	if pr == nil {
		t.logf("info", "probing %s", t.url)
		var perr error
		pr, perr = t.client.Probe(ctx, t.url)
		if perr != nil {
			t.setState(StateError)
			t.emitFinal(progressSink)
			return fmt.Errorf("probe: %w", perr)
		}
		t.probe.Store(pr)
	}
	// --checksum-url: pull the digest from the sidecar before any byte of the
	// payload moves. The sidecar is a small text file ("algo:hash" or a bare
	// hash, as emitted by sha256sum et al); parsing accepts both. A fetch or
	// parse failure fails the task — silently skipping verification would
	// defeat the point of asking for it.
	if t.opts.ChecksumURL != "" && t.opts.Checksum == "" {
		spec, err := t.fetchChecksumSpec(ctx)
		if err != nil {
			t.setState(StateError)
			t.emitFinal(progressSink)
			return fmt.Errorf("checksum-url: %w", err)
		}
		t.opts.Checksum = spec
		t.logf("info", "checksum from %s: %s", t.opts.ChecksumURL, spec)
	}
	// Smart profile: decide the concrete engine now that the probe answered
	// range support + size, and check h2 readiness through the h2 client.
	// The CLI path already resolved smart to a concrete profile (SetProfile,
	// after the probe pass) and injected it, so this only runs for tasks whose
	// profile is still literally "smart" — the RPC daemon path where Start
	// probes lazily.
	if t.opts.Profile == "smart" {
		// The scheduler applies the Balancer's per-file allocation via
		// SetConns BEFORE Start runs, so t.conns holds this file's real budget
		// (1 in batch mode) — using the raw Start `conns` (the global default
		// from the TaskMaker) would make smart choose "both" for a batch file
		// that actually gets 1 connection. Fall back to the parameter when
		// nothing was set (Manager.Run's direct path).
		perFile := int(t.conns.Load())
		if perFile < 1 {
			perFile = conns
		}
		profile, reason := ChooseProfile(ServerCapabilities{
			TotalSize:     pr.TotalSize,
			SupportsRange: pr.SupportsRange,
			SingleStream:  pr.SingleStream,
			HTTP2Ready:    t.h2Client != nil && t.h2Client.SupportsHTTP2(ctx, t.url),
			Conns:         perFile,
		})
		t.logf("info", "smart profile: chose %q (%s)", profile, reason)
		t.opts.Profile = profile
	}
	if pr.Filename == "" || (t.opts.OutputName != "") {
		// Filename refinement publishes a COPIED probe: pr is shared with
		// Snapshot() readers, and in-place mutation raced with them.
		pr2 := *pr
		if pr2.Filename == "" {
			pr2.Filename = deriveFilename(pr2.FinalURL, t.opts.OutputName)
		}
		if t.opts.OutputName != "" {
			pr2.Filename = t.opts.OutputName // explicit -o wins
		}
		t.probe.Store(&pr2)
		pr = &pr2
	}
	t.setState(StateActive)

	// 2. Resolve paths + attempt resume.
	dir := t.opts.Dir
	outName := flattenFilename(pr.Filename)
	if outName == "" {
		outName = "download.bin"
	}
	t.outPath = filepath.Join(dir, outName)

	// Collision policy — applies only when this run is NOT resuming: a
	// resumable .odm control file owns this destination and must never be
	// renamed away from it. (--continue is on by default, so gate on the
	// control file's presence, not on the flag.)
	resumable := false
	if t.opts.Continue {
		if _, cerr := storage.LoadControl(t.outPath); cerr == nil {
			resumable = true
		}
	}
	if !resumable {
		switch t.opts.Collision {
		case "skip":
			if st, err := os.Stat(t.outPath); err == nil && st.Mode().IsRegular() {
				// Size match when known → genuinely complete, skip as success;
				// otherwise the file exists but we can't vouch for it.
				if pr.TotalSize > 0 && st.Size() == pr.TotalSize {
					t.logf("info", "skipping %s: already downloaded (%d bytes)", outName, st.Size())
					t.bytesDone.Store(st.Size())
					t.setState(StateCompleted)
					// Emit so the skipped task appears in the final UI frame /
					// RPC state instead of vanishing (every other completion
					// path emits before returning).
					if progressSink != nil {
						progressSink(t.Snapshot())
					}
					return nil
				}
				t.logf("warn", "--skip-existing: %s exists with a different size (%d ≠ %s), re-downloading", outName, st.Size(), sizeOrUnknown(pr.TotalSize))
			}
		case "rename":
			if _, err := os.Stat(t.outPath); err == nil {
				outName = uniqueName(dir, outName)
				// Publish via a COPIED probe: pr is shared with Snapshot()
				// readers (UI/RPC pollers), and mutating pr.Filename in place
				// raced with them. Same for deriveFilename below.
				pr2 := *pr
				pr2.Filename = outName
				t.probe.Store(&pr2)
				pr = &pr2
				t.outPath = filepath.Join(dir, outName)
				t.logf("info", "%s exists — saving as %s", filepath.Base(t.outPath), outName)
			}
		}
	}

	// Probe-derived size check before opening. A SingleStream verdict means the
	// server won't honour ranged GETs, so the queue MUST hold exactly one
	// whole-file chunk regardless of the known size — otherwise worker N would
	// pull chunk N, the server answers the ranged request with the full body,
	// and that body gets written at the chunk's offset, corrupting the file
	// (and over-counting bytesDone past TotalSize, which used to let the task
	// "succeed" on the corrupt output). NewChunkQueue's totalSize<0 branch is
	// exactly the single whole-file chunk layout we want.
	qs := pr.TotalSize
	if pr.SingleStream {
		qs = -1
	}
	// Engine profile: aria2c splits the file into `split` segments of roughly
	// equal size (bounded by min-split-size), odm uses fixed-size chunks with
	// work-stealing. both splits the file into two regions — [0, mid) via the
	// odm engine (h1 client), [mid, end) via the aria2c engine (h2 client).
	// The layout is deterministic from (TotalSize, split params), so resume
	// rebuilds it from the control file.
	effective := t.opts.ChunkSize
	isAria := t.opts.Profile == "aria2c"
	isBoth := t.opts.Profile == "both"
	t.layoutMu.Lock()
	t.engines = nil
	t.single = nil
	t.splitAt = 0
	if isAria && qs > 0 {
		n, seg := AriaSplit(qs, int64(t.opts.Split), t.opts.MinSplitSize)
		effective = seg
		t.ariaSplit = n
		t.logf("info", "aria2c profile: %d segments of ~%s each", n, formatSegSize(seg))
	}
	if isBoth && qs > 0 && qs < 4*1024*1024 {
		// Tiny file: a 50/50 split gains nothing (the aria2c region would be
		// a couple of segments at most). Degrade to the plain odm engine.
		isBoth = false
		t.logf("info", "both profile: file < 4 MiB, using single-region odm engine")
	}
	if isBoth && qs > 0 {
		// both: region1 [0, mid) = odm fixed-chunk work-stealing (h1 client),
		// region2 [mid, end) = aria2c static split (h2 client). Connection
		// budget halves; a single connection or tiny file degrades to the odm
		// engine (see below).
		if conns < 2 {
			// One connection can't split across two regions without doubling
			// the TCP budget (max(1,1)+max(1,0) would spawn 2 workers on a
			// 1-connection budget). Degrade to the single-region odm engine.
			isBoth = false
			t.logf("info", "both profile: %d connection(s), using single-region odm engine", conns)
		}
	}
	if isBoth && qs > 0 {
		conns1 := max(1, conns/2)
		conns2 := max(1, conns-conns1)
		t.regionConns = []int{conns1, conns2}
		t.splitAt = qs / 2
		if t.splitAt < 1 {
			t.splitAt = 1
		}
		mid := t.splitAt
		n2, _ := AriaSplit(qs-mid, int64(t.opts.Split), t.opts.MinSplitSize)
		t.engines = []*Engine{
			// region1 = odm work-stealing → h1 (t.client is ALWAYS the h1
			// client now); region2 = static split → h2 when available.
			{q: NewChunkQueue(mid, t.opts.ChunkSize), client: t.client, base: 0},
			{q: NewStaticQueue(qs-mid, n2), client: t.engineClient(true), base: mid},
		}
		t.ariaSplit = n2
		t.logf("info", "both profile: region1 [0,%d) odm (%d conns, h1), region2 [%d,%d) aria2c (%d segments, h2)",
			mid, conns1, mid, qs, n2)
	}
	var q workQueue
	if t.engines != nil {
		q = t.engines[0].q
	} else if isAria && qs > 0 {
		q = NewStaticQueue(qs, t.ariaSplit)
	} else {
		q = NewChunkQueue(qs, effective)
	}
	t.queue = q         // set early: resume restore/verification below reads the queue
	t.layoutMu.Unlock() // layout settled; readers (currentEngine) may proceed

	alreadyDone := int64(0)
	var controlFile *storage.ControlFile
	if t.opts.Continue {
		if cf, cerr := storage.LoadControl(t.outPath); cerr == nil {
			controlFile = cf
			// ETag validation: if both are non-empty and don't match, the file
			// changed on the server — do NOT resume stale chunks.
			if cf.ETag != "" && pr.ETag != "" && cf.ETag != pr.ETag {
				t.logf("warn", "ETag changed (%s → %s), re-downloading from scratch", cf.ETag, pr.ETag)
			} else if cf.TotalSize == pr.TotalSize &&
				cf.ChunkSize == effective &&
				(cf.Profile == "" || cf.Profile == t.opts.Profile) &&
				cf.SplitAt == t.splitAt &&
				cf.Region2ChunkSize == t.region2ChunkSize() {
				var ok bool
				offs := cf.CompletedOffsets()
				if t.engines != nil {
					// Split the absolute offsets per region: region1 keeps them,
					// region2 subtracts its base (the queue is 0-based there).
					done1 := map[int64]struct{}{}
					done2 := map[int64]struct{}{}
					for off := range offs {
						if off >= t.splitAt {
							done2[off-t.splitAt] = struct{}{}
						} else {
							done1[off] = struct{}{}
						}
					}
					var a1, a2 int64
					a1, ok = t.engines[0].ResetCompletedOffsets(done1, t.splitAt)
					a2, _ = t.engines[1].ResetCompletedOffsets(done2, t.engines[1].base)
					alreadyDone = a1 + a2
				} else {
					alreadyDone, ok = q.ResetCompletedOffsets(offs, pr.TotalSize)
				}
				if !ok {
					// e.g. a ranged control file now hitting a single-stream URL,
					// or a stale layout. Trust nothing, re over.
					t.logf("warn", "control file layout doesn't match this download, re-downloading from scratch")
					alreadyDone = 0
				} else {
					// Pin the control file's ETag for If-Range on every ranged
					// GET this run (empty when the server never sent one).
					t.resumeETag = cf.ETag
					t.bytesDone.Store(alreadyDone)
					// Carry the recorded hashes into this run so checkpoints keep
					// persisting them (otherwise the next resume would silently
					// downgrade to the legacy server-compare fallback).
					t.restoreChunkHashes(cf)
					t.logf("info", "resuming %s: %d bytes already written", outName, alreadyDone)
				}
			}
		}
	}

	disk, err := storage.OpenForWrite(dir, outName, pr.TotalSize)
	if err != nil {
		t.setState(StateError)
		t.emitFinal(progressSink)
		return err
	}
	t.disk = disk
	defer disk.Close()

	if alreadyDone > 0 {
		// Resume integrity check: verify the completed chunks are intact before
		// trusting them. Two complementary checks — per-chunk SHA-256 hashes
		// verify every completed chunk against local disk (catches local
		// corruption), and the sampled server-side compare detects server drift
		// (a same-size replacement the ETag check can't see). Legacy control
		// files (no hashes) rely on the server compare alone. Any mismatch →
		// full re-download.
		if err := t.verifyResumedData(ctx, controlFile); err != nil {
			t.logf("warn", "resume integrity check failed (%v) — re-downloading from scratch", err)
			alreadyDone = 0
			t.bytesDone.Store(0)
			t.clearChunkHashes()
			if t.engines != nil {
				// Rebuild both engines with the same layout math (region1 h1,
				// region2 h2-when-available — same routing as fresh Start).
				mid := t.splitAt
				n2, _ := AriaSplit(qs-mid, int64(t.opts.Split), t.opts.MinSplitSize)
				t.engines = []*Engine{
					{q: NewChunkQueue(mid, t.opts.ChunkSize), client: t.client, base: 0},
					{q: NewStaticQueue(qs-mid, n2), client: t.engineClient(true), base: mid},
				}
				q = t.engines[0].q
			} else if isAria && qs > 0 {
				q = NewStaticQueue(qs, t.ariaSplit)
			} else {
				q = NewChunkQueue(qs, effective)
			}
			t.queue = q
		}
	}

	if pr.TotalSize >= 0 && alreadyDone >= pr.TotalSize && pr.TotalSize > 0 {
		// Already complete on disk (resume found everything done).
		t.bytesDone.Store(pr.TotalSize)
		t.setState(StateCompleted)
		if err := t.verifyChecksum(); err != nil {
			t.logf("error", "checksum: %v", err)
			t.setState(StateError)
			t.emitFinal(progressSink)
			return err
		}
		t.emitFinal(progressSink)
		return t.finish()
	}

	// 3. Launch workers.
	// Don't clobber a connTarget an RPC changeOption already raised before
	// Start ran: keep the larger of (param, current target).
	if cur := t.connTarget.Load(); int(cur) > conns {
		conns = int(cur)
	}
	t.connTarget.Store(int32(conns))
	if pr.TotalSize <= 0 && pr.SingleStream {
		// Single-stream fallback: exactly one worker on the single whole-file chunk.
		conns = 1
		t.conns.Store(1)
		t.connTarget.Store(1)
	}

	// Write the control file immediately so it's visible on disk from the
	// start (like aria2's .aria2), not only after the first checkpoint.
	t.persistControl()

	// aria2c profile: cap the concurrent workers at the effective split count
	// (fewer segments than conns means some conns idle — aria2c behaves the
	// same) and at MaxConnPerServer for h1 (each stream is a separate TCP
	// connection there). For h2 the -x cap is irrelevant: all streams share
	// one connection.
	workerCount := conns
	if isAria {
		if t.ariaSplit > 0 && int64(workerCount) > t.ariaSplit {
			workerCount = int(t.ariaSplit)
		}
		if pr.TotalSize > 0 && !t.profileUsesH2() && t.opts.MaxConnPerServer > 0 && workerCount > t.opts.MaxConnPerServer {
			workerCount = t.opts.MaxConnPerServer
		}
		// The displayed/live connection count must reflect the CAP, not the raw
		// budget: with 4 segments and -c 16 the UI used to show [x16] while only
		// 4 workers existed.
		conns = workerCount
	}
	t.conns.Store(int32(conns))
	t.connTarget.Store(int32(conns))
	if t.engines != nil {
		// both profile: spawn per-region workers with the region's conns.
		for ei, eng := range t.engines {
			n := t.regionConns[ei]
			for i := 0; i < n; i++ {
				t.workerWg.Add(1)
				go t.worker(ctx, eng, &t.workerWg, progressSink)
			}
		}
	} else {
		engine := t.currentEngine()
		for range workerCount {
			t.workerWg.Add(1)
			go t.worker(ctx, engine, &t.workerWg, progressSink)
		}
	}
	// progress ticker even in single-worker sizeless case.
	t.workerWg.Wait()
	t.adjustMu.Lock()
	t.adjustDone = true
	t.adjustMu.Unlock()

	// A cancelled context (^C / SIGTERM) is not an error: partial bytes are
	// preserved and --continue resumes. Paint the task as paused instead of
	// the red error glyph so the final screen matches the "cancelled" summary.
	if err := ctx.Err(); err != nil {
		t.setState(StatePaused)
		t.persistControl()
		t.emitFinal(progressSink)
		return err
	}
	if t.errors.Load() > 0 {
		t.setState(StateError)
		// Persist completed chunks before exiting so partial progress
		// survives even if the process terminates shortly after this
		// returns (e.g. via os.Exit on signal).
		t.persistControl()
		t.emitFinal(progressSink)
		return fmt.Errorf("task failed: %d chunk errors, %d/%d bytes", t.errors.Load(), t.bytesDone.Load(), t.totalOrDone())
	}
	t.setState(StateCompleted)
	// Checksum verification runs against the actual written file. A mismatch
	// fails the task even though the transfer itself finished.
	if err := t.verifyChecksum(); err != nil {
		t.logf("error", "checksum: %v", err)
		t.setState(StateError)
		t.persistControl()
		t.emitFinal(progressSink)
		return err
	}
	t.emitFinal(progressSink)
	return t.finish()
}
