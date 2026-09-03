package scheduler

import (
	"testing"

	"odm/internal/download"
)

func TestRebalanceTargets_SingleRemainingGetsFullBudget(t *testing.T) {
	inputs := []rebalanceInput{
		{id: download.TaskID("odm-1"), rangeCapable: true},
		{id: download.TaskID("odm-2"), rangeCapable: true},
	}

	got := rebalanceTargets(inputs, 25)
	want := []int{13, 12}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("initial target[%d]: want %d got %d (%v)", i, want[i], got[i], got)
		}
	}

	remaining := []rebalanceInput{
		{id: download.TaskID("odm-2"), rangeCapable: true},
	}
	got = rebalanceTargets(remaining, 25)
	if len(got) != 1 || got[0] != 25 {
		t.Fatalf("single remaining task: want [25], got %v", got)
	}
}

func TestRebalanceTargets_NonRangeSurplusGoesToRange(t *testing.T) {
	inputs := []rebalanceInput{
		{id: download.TaskID("odm-1"), rangeCapable: false},
		{id: download.TaskID("odm-2"), rangeCapable: true},
	}

	got := rebalanceTargets(inputs, 25)
	want := []int{1, 24}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target[%d]: want %d got %d (%v)", i, want[i], got[i], got)
		}
	}
}

func TestRebalanceTargets_EvenlyDistributesRangeBudget(t *testing.T) {
	inputs := []rebalanceInput{
		{id: download.TaskID("odm-1"), rangeCapable: true},
		{id: download.TaskID("odm-2"), rangeCapable: true},
		{id: download.TaskID("odm-3"), rangeCapable: true},
	}

	got := rebalanceTargets(inputs, 10)
	want := []int{4, 3, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target[%d]: want %d got %d (%v)", i, want[i], got[i], got)
		}
	}
}

func TestRebalanceTargets_AllNonRangeKeepsSingleStream(t *testing.T) {
	inputs := []rebalanceInput{
		{id: download.TaskID("odm-1"), rangeCapable: false},
		{id: download.TaskID("odm-2"), rangeCapable: false},
	}

	got := rebalanceTargets(inputs, 25)
	want := []int{1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target[%d]: want %d got %d (%v)", i, want[i], got[i], got)
		}
	}
}

func TestRebalanceTargets_ZeroAndInsufficientBudget(t *testing.T) {
	inputs := []rebalanceInput{
		{id: download.TaskID("odm-1"), rangeCapable: true},
		{id: download.TaskID("odm-2"), rangeCapable: true},
	}

	if got := rebalanceTargets(nil, 25); len(got) != 0 {
		t.Fatalf("nil inputs: want empty target, got %v", got)
	}
	if got := rebalanceTargets(inputs, 1); len(got) != 2 || got[0] != 0 || got[1] != 0 {
		t.Fatalf("insufficient budget: want [0 0], got %v", got)
	}
}
