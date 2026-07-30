package unsupported

import (
	"testing"
	"testing/synctest"
)

// TestBenignDataRace is a data race with NO logical error: two goroutines write
// the SAME value to x with no synchronization, so whatever the interleaving, x
// ends up 1 and every assertion holds. weave reports only wrong results, panics,
// deadlocks, and leaks — never the mere absence of synchronization — so it
// explores the interleavings and PASSES.
//
// This is not a weave defect but its complement to `-race`: `-race` WOULD flag
// the unsynchronized access (a real data race that is undefined behavior under
// the Go memory model, even though this instance is benign), whereas weave asks
// "does any schedule compute a wrong answer / deadlock". Run both: `-race` for
// missing happens-before edges, weave for schedule-dependent logic bugs.
func TestBenignDataRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := 0
		go func() { x = 1 }()
		go func() { x = 1 }()
		synctest.Wait()
		if x != 1 {
			t.Fatalf("x=%d", x)
		}
	})
}
