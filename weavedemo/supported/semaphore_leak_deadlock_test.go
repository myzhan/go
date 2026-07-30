package supported

import (
	"testing"
	"testing/synctest"
)

// TestSemaphoreLeakDeadlock is a resource-leak deadlock: a capacity-1 channel is
// used as a semaphore holding a single permit (send = acquire, receive =
// release). Worker A acquires the permit, but on the cancellation path it returns
// early WITHOUT releasing — the classic "forgot to unlock on an error return"
// bug (the fix is `defer func() { <-slot }()`). Worker B sets the cancelled flag
// and then needs the permit for its own work; if A already leaked it, B's acquire
// blocks forever and the bubble deadlocks.
//
// It is schedule-dependent. Serialized, worker A runs to completion first: it
// sees cancelled == false, so it releases the permit normally, and B then
// acquires it fine — no deadlock (so a single synctest schedule PASSES). But in
// the interleaving where B sets cancelled BEFORE A checks it, A takes the early
// return and leaks the permit, so B blocks forever. weave explores the
// interleavings, finds that one, and reports the deadlock with a reproducing
// seed. EXPECTED TO FAIL under -weave.
func TestSemaphoreLeakDeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slot := make(chan struct{}, 1) // a semaphore with one permit
		cancelled := false

		go func() { // worker A
			slot <- struct{}{} // acquire the permit
			if cancelled {
				return // BUG: returns without releasing (should defer <-slot)
			}
			<-slot // release the permit
		}()

		go func() { // worker B: cancels, then needs the permit itself
			cancelled = true
			slot <- struct{}{} // acquire — blocks forever if A leaked the permit
			<-slot             // release
		}()

		synctest.Wait()
	})
}
