package supported

import (
	"testing"
	"testing/synctest"
)

// TestSemaphoreOversubscribe is a check-then-act bug in a hand-rolled
// concurrency limiter: `active < max` (check) and `active++` (act) are not
// atomic, so more goroutines than the limit can pass the check before any
// increments, exceeding the cap. With max=2 and three workers, an interleaving
// where all three read active < 2 before incrementing drives the peak to 3.
// weave turns the reads/writes of the counter into scheduling points and finds
// it. EXPECTED TO FAIL under -weave. The fix is a real semaphore (a buffered
// channel of capacity max, or a mutex around check+increment).
func TestSemaphoreOversubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const max = 2
		active := 0
		peak := 0
		acquire := func() {
			if active < max { // check
				active++ // act (not atomic with the check)
			}
			if active > peak {
				peak = active
			}
		}
		for range 3 {
			go acquire()
		}
		synctest.Wait()
		if peak > max {
			t.Fatalf("over-subscribed: peak=%d, limit=%d", peak, max)
		}
	})
}
