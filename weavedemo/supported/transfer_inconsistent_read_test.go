package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestTransferInconsistentRead is an atomicity-violation / inconsistent-read
// bug. A transfer moves 10 from account a to account b, but each account is
// guarded by its OWN lock, so the two-step transfer is not atomic as a whole. A
// concurrent observer that reads a and b (each under its own lock) can catch the
// system mid-transfer — after a was debited but before b was credited — and see
// a total of 190 instead of the invariant 200. Every individual access is
// correctly locked (so -race sees nothing wrong), yet the composite invariant is
// still broken. weave explores the interleavings and finds the observer's
// inconsistent read. EXPECTED TO FAIL under -weave.
func TestTransferInconsistentRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var muA, muB sync.Mutex
		a, b := 100, 100
		go func() { // transfer 10 from a to b, one lock at a time
			muA.Lock()
			a -= 10
			muA.Unlock()
			muB.Lock()
			b += 10
			muB.Unlock()
		}()
		go func() { // observer: reads each balance under its own lock
			muA.Lock()
			va := a
			muA.Unlock()
			muB.Lock()
			vb := b
			muB.Unlock()
			if va+vb != 200 {
				panic("observer saw inconsistent total")
			}
		}()
		synctest.Wait()
	})
}
