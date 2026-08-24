package download

import (
	"testing"
	"time"
)

// newRoutingManager builds a real Manager exactly like the CLI/RPC paths do,
// for the given profile. Returns the manager and its task (via NewTask, the
// production wiring) so the tests pin the CONTRACT, not internals.
func newRoutingManager(t *testing.T, profile string) (*Manager, *Task) {
	t.Helper()
	m, err := NewManager(ExecOptions{
		Dir:         t.TempDir(),
		Connections: 4,
		Timeout:     5 * time.Second,
		Profile:     profile,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := m.NewTask("http://example.invalid/f.bin", 0)
	if err != nil {
		t.Fatal(err)
	}
	return m, task
}

// TestClientRouting_PrimaryIsAlwaysH1 pins the M5 contract: a Task's primary
// client is the H1-ONLY client for every profile. The odm engine (fixed-chunk
// work-stealing — region1 of both, and every degraded/single-region path)
// derives its value from N parallel TCP connections; handing those tasks an
// h2-enabled client collapses all N workers into ONE TCP stream and silently
// wastes the connection budget. Only the static-split (aria2c-model) engines
// may route onto the h2 client, where streams multiplex over one connection
// by design.
func TestClientRouting_PrimaryIsAlwaysH1(t *testing.T) {
	for _, profile := range []string{"", "odm", "aria2c", "both", "smart"} {
		m, task := newRoutingManager(t, profile)
		if task.client != m.Client() {
			t.Errorf("profile %q: primary client must be the h1-only client", profile)
		}
	}
}

// TestClientRouting_H2AttachedOnlyWhereUsed pins Manager-side attachment: the
// h2 client is built and attached for profiles whose static-split engines can
// use it (aria2c/both/smart) and NOT for plain odm.
func TestClientRouting_H2AttachedOnlyWhereUsed(t *testing.T) {
	_, odm := newRoutingManager(t, "odm")
	if odm.h2Client != nil {
		t.Error("odm profile must not carry an h2 client")
	}
	_, aria := newRoutingManager(t, "aria2c")
	if aria.h2Client == nil {
		t.Error("aria2c profile must carry an h2 client")
	}
	_, both := newRoutingManager(t, "both")
	if both.h2Client == nil {
		t.Error("both profile must carry an h2 client")
	}
}

// TestClientRouting_SingleEngineFollowsQueueKind pins currentEngine's routing
// rule: the hoisted single engine speaks h2 ONLY when its queue is a
// StaticQueue (aria2c model). A ChunkQueue under a "both"-degraded task (tiny
// file or conns<2 collapse to the odm engine while opts.Profile still reads
// "both") must stay on h1 — the profile string lies about the engine kind;
// the queue type doesn't.
func TestClientRouting_SingleEngineFollowsQueueKind(t *testing.T) {
	_, aria := newRoutingManager(t, "aria2c")
	aria.opts.Profile = "aria2c"
	aria.queue = NewStaticQueue(10<<20, 4)
	eng := aria.currentEngine()
	if eng.Client() != aria.h2Client {
		t.Error("static-split single engine must ride the h2 client")
	}

	mDeg, deg := newRoutingManager(t, "both")
	deg.opts.Profile = "both" // degraded: queue is odm-style despite the profile
	deg.queue = NewChunkQueue(10<<20, 512)
	engDeg := deg.currentEngine()
	if engDeg.Client() != mDeg.Client() {
		t.Error("degraded both (ChunkQueue) must stay on the h1-only client")
	}

	mPlain, plain := newRoutingManager(t, "odm")
	plain.queue = NewChunkQueue(10<<20, 512)
	if plain.currentEngine().Client() != mPlain.Client() {
		t.Error("plain odm engine must stay on the h1-only client")
	}
}
