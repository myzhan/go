package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestInconsistentLocking is a lost update whose root cause is subtler than "no
// lock at all": both goroutines DO take a lock around x = x + 1 — but they take
// DIFFERENT mutexes (mu1 vs mu2). Two different locks provide no mutual exclusion
// against each other, so both goroutines can read x, then both write, and one
// increment is lost. The lesson: adding a lock does not make code thread-safe;
// every access to a piece of shared state must be guarded by the SAME lock. This
// is a common real-world bug (e.g. a refactor introduces a second lock, or a
// per-object lock is used where a shared one was needed).
//
// weave turns the reads/writes of x into scheduling points (the mutexes, being
// distinct, impose no ordering between the two goroutines) and finds the
// interleaving that leaves x == 1. EXPECTED TO FAIL under -weave; PASSES in a
// single serial synctest schedule. The fix is to use one mutex for all accesses
// to x. (-race also flags it: guarding with different locks still leaves the
// accesses unsynchronized relative to each other.)
func TestInconsistentLocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu1, mu2 sync.Mutex
		x := 0
		go func() {
			mu1.Lock()
			x = x + 1
			mu1.Unlock()
		}()
		go func() {
			mu2.Lock() // WRONG: a different lock — no exclusion against mu1
			x = x + 1
			mu2.Unlock()
		}()
		synctest.Wait()
		if x != 2 {
			t.Fatalf("lost update under inconsistent locking: x=%d", x)
		}
	})
}
