package keeper

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestCombinedChecksOnlyGivenTasks(t *testing.T) {
	t.Parallel()
	cmd, runs := countingGate(t, "test ! -e b.txt")
	e := combinedSetup(t, cmd, []string{"a", "b", "c"}, nil)
	e.o.GateSlotPoll = time.Millisecond
	// Named against plan order: the check keeps plan order.
	e.o.Tasks = []string{e.ids[2], e.ids[0]}
	want := []string{e.ids[0], e.ids[2]}
	ctx := context.Background()

	first, err := Combined(ctx, e.o)
	if err != nil {
		t.Fatal(err)
	}
	// The gate is red whenever b is merged, so green shows b was left out.
	if !first.Ran || !first.Passed || first.Cached || !slices.Equal(first.Branches, want) {
		t.Fatalf("first = %+v, want branches %v\n%s", first, want, e.out)
	}
	after := runs()

	second, err := Combined(ctx, e.o)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Passed || !second.Cached || second.Probes != 0 || runs() != after || !slices.Equal(second.Branches, want) {
		t.Fatalf("second = %+v, gate runs %d -> %d", second, after, runs())
	}
	e.assertNoThrowaway(t)
}
