package scheduler

import (
	"strings"
	"testing"
)

// mkRange builds N file inputs that all support ranged requests.
func mkRange(n int) []FileInput {
	out := make([]FileInput, n)
	for i := range out {
		out[i] = FileInput{URL: "u", SupportsRange: true}
	}
	return out
}

func totalConns(p *Plan) int {
	t := 0
	for _, a := range p.Parallel {
		t += a.Connections
	}
	return t
}

func TestModeA_AllBudgetToSingleFile(t *testing.T) {
	p, err := Compute(16, mkRange(1), 0, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 1 || p.Parallel[0].Connections != 16 {
		t.Fatalf("want 1 file with 16 conns, got %+v", p.Parallel)
	}
	if len(p.Queued) != 0 {
		t.Fatalf("want 0 queued, got %d", len(p.Queued))
	}
}

func TestModeA_CappedByMax(t *testing.T) {
	p, err := Compute(100, mkRange(1), 0, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Parallel[0].Connections != 32 {
		t.Fatalf("want cap 32, got %d", p.Parallel[0].Connections)
	}
	if p.Warning == "" {
		t.Fatalf("want warning for C>max, got none")
	}
}

func TestModeA_NoRangeSupportSingleStream(t *testing.T) {
	files := []FileInput{{URL: "u", SupportsRange: false}}
	p, err := Compute(16, files, 0, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Parallel[0].Connections != 1 {
		t.Fatalf("want single-stream fallback of 1, got %d", p.Parallel[0].Connections)
	}
}

func TestModeB_FilesInParallelQueued(t *testing.T) {
	// 16 urls, C=16 → 16 parallel, 0 queued, 1 conn each.
	p, err := Compute(16, mkRange(16), 0, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 16 || len(p.Queued) != 0 {
		t.Fatalf("want 16 parallel/0 queued, got %d/%d", len(p.Parallel), len(p.Queued))
	}
	for _, a := range p.Parallel {
		if a.Connections != 1 {
			t.Fatalf("want 1 conn/file, got %d", a.Connections)
		}
	}
}

func TestModeB_MoreUrlsThanBudget(t *testing.T) {
	// 16 urls, C=8 → 8 parallel, 8 queued.
	p, err := Compute(8, mkRange(16), 0, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 8 || len(p.Queued) != 8 {
		t.Fatalf("want 8/8, got %d/%d", len(p.Parallel), len(p.Queued))
	}
}

func TestModeB_CappedByMaxConnections(t *testing.T) {
	// N=64, C=64, max=32 → parallel capped at 32.
	p, err := Compute(64, mkRange(64), 0, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 32 || len(p.Queued) != 32 {
		t.Fatalf("want 32 parallel/32 queued, got %d/%d", len(p.Parallel), len(p.Queued))
	}
}

func TestModeC_SpecExample(t *testing.T) {
	// spec example: -c 16 -sf 5, 10 urls (all range-support).
	// parallel_files = floor(16/5)=3, used=15, remainder=1
	// → file#1:6, file#2:5, file#3:5, total 16; 7 queued.
	p, err := Compute(16, mkRange(10), 5, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 3 || len(p.Queued) != 7 {
		t.Fatalf("want 3 parallel/7 queued, got %d/%d", len(p.Parallel), len(p.Queued))
	}
	got := []int{p.Parallel[0].Connections, p.Parallel[1].Connections, p.Parallel[2].Connections}
	want := []int{6, 5, 5}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("conns[%d]: want %d got %d (%v)", i, want[i], got[i], got)
		}
	}
	if totalConns(p) != 16 {
		t.Fatalf("want total 16, got %d", totalConns(p))
	}
}

func TestModeC_RemainderDistribution(t *testing.T) {
	// -c 10 -sf 3: floor(10/3)=3, used=9, remainder=1 → conns=[4,3,3].
	p, err := Compute(10, mkRange(6), 3, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := []int{p.Parallel[0].Connections, p.Parallel[1].Connections, p.Parallel[2].Connections}
	want := []int{4, 3, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("conns[%d]: want %d got %d (%v)", i, want[i], got[i], got)
		}
	}
}

func TestModeC_SFExceedsParallelUsesFullN(t *testing.T) {
	// -c 16 -sf 5, 3 urls: floor(16/5)=3 but N=3 → 3 parallel, used=15,
	// remainder=1 → [6,5,5], 0 queued, total 16.
	p, err := Compute(16, mkRange(3), 5, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 3 || len(p.Queued) != 0 {
		t.Fatalf("want 3/0, got %d/%d", len(p.Parallel), len(p.Queued))
	}
	if totalConns(p) != 16 {
		t.Fatalf("want total 16, got %d", totalConns(p))
	}
}

func TestModeC_QueuedInheritsSF(t *testing.T) {
	// -c 10 -sf 3, 8 urls: 3 parallel (4/3/3), 5 queued — each queued file
	// must inherit SF=3 (the Scheduler's documented "Mode C: SF" contract),
	// not the Mode-B default of 1.
	p, err := Compute(10, mkRange(8), 3, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.Parallel) != 3 || len(p.Queued) != 5 {
		t.Fatalf("want 3/5, got %d/%d", len(p.Parallel), len(p.Queued))
	}
	for i, a := range p.Queued {
		if a.Connections != 3 {
			t.Fatalf("queued[%d] want 3 (SF), got %d", i, a.Connections)
		}
	}
	// A queued single-stream file caps at 1.
	files := []FileInput{
		{URL: "a", SupportsRange: true}, {URL: "b", SupportsRange: true},
		{URL: "c", SupportsRange: false},
	}
	p2, err := Compute(6, files, 2, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for i, a := range p2.Queued {
		if files[2+i].SupportsRange == false && a.Connections != 1 {
			t.Fatalf("queued single-stream %d want 1, got %d", i, a.Connections)
		}
	}
}

func TestValidation_CBelowOne(t *testing.T) {
	if _, err := Compute(0, mkRange(1), 0, 32); err == nil {
		t.Fatalf("want error for C=0")
	}
}

func TestValidation_SFGreaterThanC(t *testing.T) {
	_, err := Compute(4, mkRange(8), 5, 32)
	if err == nil || !strings.Contains(err.Error(), "cannot be greater") {
		t.Fatalf("want SF>C error, got %v", err)
	}
}

func TestValidation_SFIgnoredInSingleFile(t *testing.T) {
	p, err := Compute(16, mkRange(1), 4, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// SF ignored → full budget to the one file, no error.
	if p.Parallel[0].Connections != 16 {
		t.Fatalf("want 16 (sf ignored), got %d", p.Parallel[0].Connections)
	}
	if !strings.Contains(p.Warning, "ignored in single-file mode") {
		t.Fatalf("want sf-ignored warning, got %q", p.Warning)
	}
}

func TestAllocationTimeReallocation(t *testing.T) {
	//: a non-range parallel file is capped to 1 and its freed budget is
	// redistributed to the other parallel files.
	// -c 16 -sf 5, 10 urls, files #1..#3 are parallel (floor(16/5)=3).
	//   initial after remainder: [6,5,5] (total 16).
	//   make file#1 (index 0) non-range: freed = 6-1 = 5. Redistribute one at
	//   a time to files [1,2] only (file#0 itself excluded): 5 tokens over 2
	//   files → [1,8,7]? Actually first fills file#1 +1 →7 (+1)... let's just
	//   assert: file#0 capped to 1, others increased, total stays 16, and file#0 never gets extra.
	files := make([]FileInput, 10)
	for i := range files {
		files[i] = FileInput{URL: "u", SupportsRange: true}
	}
	files[0].SupportsRange = false
	p, err := Compute(16, files, 5, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.Parallel[0].Connections != 1 {
		t.Fatalf("non-range file must be capped to 1, got %d", p.Parallel[0].Connections)
	}
	if totalConns(p) != 16 {
		t.Fatalf("freed budget must be redistributed; total want 16 got %d", totalConns(p))
	}
}

func TestAllocationTimeReallocation_NeverAddsToNonRange(t *testing.T) {
	// All three parallel files non-range: each capped to 1, nothing to
	// redistribute to (no range files in the parallel set), so the surplus is
	// simply unused. Total = 3.
	files := make([]FileInput, 10)
	for i := range files {
		files[i] = FileInput{URL: "u", SupportsRange: false}
	}
	p, err := Compute(16, files, 5, 32)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for i, a := range p.Parallel {
		if a.Connections != 1 {
			t.Fatalf("parallel[%d] want 1 (single-stream), got %d", i, a.Connections)
		}
	}
	if totalConns(p) != 3 {
		t.Fatalf("all-non-range: total want 3 got %d", totalConns(p))
	}
}
