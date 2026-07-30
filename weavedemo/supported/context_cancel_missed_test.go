package supported

import (
	"context"
	"testing"
	"testing/synctest"
)

// TestContextCancelMissed is a check-then-act (TOCTOU) bug against a context that
// leaks a goroutine. The worker POLLS ctx.Err() to decide it is not cancelled,
// and only then commits to a blocking receive on the work channel. The check and
// the block are not atomic: if cancellation lands in between, the worker has
// already decided to wait for work that (because the coordinator stops on cancel)
// will never arrive — so it blocks forever, a leaked goroutine.
//
// This is the canonical reason to `select` on ctx.Done() at the SAME point you
// block, rather than polling ctx.Err() and then blocking separately:
//
//	select {
//	case v := <-work: ...
//	case <-ctx.Done(): return   // the fix
//	}
//
// It is schedule-dependent. In a single synctest schedule the cancel runs before
// the worker's check, so the worker sees ctx.Err() != nil and skips the wait — no
// leak (PASSES). weave explores the interleaving where cancel lands AFTER the
// check but before/around the receive, finds the worker stuck on the channel, and
// reports the deadlock (all goroutines blocked; g blocked on chan recv) with a
// reproducing seed. EXPECTED TO FAIL under -weave.
//
// (Distinct from the timeout goroutine-leak demo: that leaks a worker DELIVERING
// to an abandoned consumer; this leaks a worker WAITING after a missed cancel.)
func TestContextCancelMissed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		work := make(chan int) // the coordinator would deliver work here

		go func() { // worker
			if ctx.Err() == nil { // check: appears not cancelled...
				<-work // ...act: block for work — missed a cancel after the check
			}
		}()
		go func() { cancel() }() // cancellation races the check above

		synctest.Wait()
	})
}
