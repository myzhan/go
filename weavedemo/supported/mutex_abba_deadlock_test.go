package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestMutexABBADeadlock is the classic AB/BA lock-ordering deadlock in real
// sync.Mutexes: one goroutine takes a then b, the other takes b then a. In the
// interleaving where each holds one lock and waits for the other, neither can
// proceed. weave finds it and reports the deadlock with a reproducing seed.
// EXPECTED TO FAIL under -weave.
//
// Mutex operations are always scheduling points (the hook in internal/sync is
// compiled unconditionally), so this one needs no memory instrumentation.
// Compare channel_lock_deadlock_test.go (the same hazard built from channels) and
// dining_philosophers_test.go (a three-way cycle instead of two).
func TestMutexABBADeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var a, b sync.Mutex
		go func() {
			a.Lock()
			b.Lock()
			b.Unlock()
			a.Unlock()
		}()
		go func() {
			b.Lock()
			a.Lock()
			a.Unlock()
			b.Unlock()
		}()
	})
}
