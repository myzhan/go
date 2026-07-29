package supported

import (
	"testing"
	"testing/synctest"
)

// TestChannelOrderAssumption is an ordering-assumption bug: two goroutines each
// send one value into a buffered channel with no synchronization, and the
// consumer assumes 1 lands before 2. The order of the two sends is
// schedule-dependent, so 2 can arrive first. weave explores the interleavings
// and finds the schedule that reverses them, reporting the broken assumption
// with a reproducing seed. EXPECTED TO FAIL under -weave.
//
// Note: whether a single plain `go test` run happens to hit the reversed order
// depends on synctest's one schedule; the test is written to reflect the real
// concurrent code (two racing sends), not tuned so that the single-run scheduler
// makes it pass.
func TestChannelOrderAssumption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan int, 2)
		go func() { ch <- 1 }()
		go func() { ch <- 2 }()
		synctest.Wait()
		if first := <-ch; first != 1 { // wrong: the two sends may land in either order
			t.Fatalf("expected 1 first, got %d", first)
		}
	})
}
