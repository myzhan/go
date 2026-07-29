package supported

import (
	"testing"
	"testing/synctest"
)

// TestDoubleCloseGuardRace is a check-then-act (TOCTOU) whose symptom is a
// panic, not a wrong number: two goroutines each guard a close() with a shared
// `closed` flag, but the check and the set are not atomic. Serialized, the
// second call sees closed==true and skips — safe. But in the interleaving where
// both read closed==false before either sets it, both run close(ch) and the
// second panics with "close of closed channel". weave turns the flag's read and
// write into scheduling points, finds that interleaving, and CAPTURES the panic
// (panic capture is a first-class weave outcome, not just assertion failures).
// EXPECTED TO FAIL under -weave; PASSES in a single synctest schedule.
func TestDoubleCloseGuardRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan int)
		closed := false
		closeOnce := func() {
			if !closed { // check
				closed = true // set (not atomic with the check)
				close(ch)     // act: double close panics if both pass the check
			}
		}
		go closeOnce()
		go closeOnce()
		synctest.Wait()
	})
}
