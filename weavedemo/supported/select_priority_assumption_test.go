package supported

import (
	"testing"
	"testing/synctest"
)

// TestSelectPriorityAssumption is a select-fairness bug: both data and quit are
// ready when the select runs, and the code assumes the data branch is chosen.
// But Go's select picks a ready case pseudo-randomly — there is no priority — so
// the quit branch can win and the value is dropped. weave enumerates the select
// cases and deterministically finds the schedule that takes quit, reporting the
// broken assumption with a reproducing seed. EXPECTED TO FAIL under -weave.
//
// Note plain `go test` (a single synctest schedule) is FLAKY here: synctest also
// picks a ready case pseudo-randomly, so it fails intermittently — which is
// exactly the point. A single run cannot reliably reproduce the bug; weave makes
// it deterministic. Do not "fix" the flakiness by delaying quit — that would
// change what the test exercises to suit the single-run scheduler.
func TestSelectPriorityAssumption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		data := make(chan int, 1)
		quit := make(chan int, 1)
		data <- 1
		quit <- 1 // both cases are ready: no branch has priority
		got := 0
		select {
		case v := <-data:
			got = v
		case <-quit:
			got = -1 // "shutdown" path wrongly assumed to be lower priority
		}
		if got != 1 {
			t.Fatalf("assumed data branch wins, got %d", got)
		}
	})
}
