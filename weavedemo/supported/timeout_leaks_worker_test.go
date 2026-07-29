package supported

import (
	"testing"
	"testing/synctest"
)

// TestTimeoutGoroutineLeak is a classic real-world async bug: a result is
// delivered on an UNBUFFERED channel, but the consumer also has a timeout path.
// When the timeout branch is taken, the worker's `result <- v` has no receiver
// and blocks forever — a leaked goroutine. weave enumerates the select and finds
// the interleaving where the timeout wins, then reports the leak as a deadlock
// with a reproducing seed. EXPECTED TO FAIL under -weave.
//
// The timeout is pre-elapsed (a buffered channel already holding a token) so it is
// ready at the select point, modeling "the deadline already passed". A real
// time.After timeout would instead need the fake clock to advance, which today
// only happens when the whole bubble is durably blocked — see
// .claude/weave/imp.md on the timeout-vs-event false negative.
//
// CAUTION for plain (`go test` without -weave) runs of this package: a single
// synctest schedule hits the leak here deterministically, and a deadlocked bubble
// is reported by synctest as a PANIC, which aborts the whole test binary — so the
// tests declared after this one never run. That is precisely the difference weave
// makes: it reports the same bug as a test failure with the interleaving and a
// seed. To run the rest of the package without -weave, exclude this test with
// `-run`. The fix is a buffered result channel (correct_asyncio_test.go).
func TestTimeoutGoroutineLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result := make(chan int) // unbuffered: the bug
		timeout := make(chan struct{}, 1)
		timeout <- struct{}{}        // deadline already elapsed
		go func() { result <- 42 }() // worker tries to deliver
		select {
		case <-result:
		case <-timeout:
			// consumer gives up; the worker's send now leaks.
		}
	})
}
