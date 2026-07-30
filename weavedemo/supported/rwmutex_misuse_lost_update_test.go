package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestRWMutexMisuseLostUpdate is an RWMutex-misuse lost update: the increment is
// guarded by RLock/RUnlock (a SHARED read lock) instead of Lock/Unlock. A read
// lock does not exclude other readers, so two goroutines hold it at once, both
// read x, and one increment is lost. weave turns the reads/writes of x into
// scheduling points and finds the interleaving leaving x == 1. EXPECTED TO FAIL
// under -weave. The fix is rw.Lock() (the exclusive write lock) around the
// mutation.
func TestRWMutexMisuseLostUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var rw sync.RWMutex
		x := 0
		inc := func() {
			rw.RLock() // WRONG: shared lock does not serialize the writes below
			x = x + 1
			rw.RUnlock()
		}
		go inc()
		go inc()
		synctest.Wait()
		if x != 2 {
			t.Fatalf("lost update: x=%d", x)
		}
	})
}
