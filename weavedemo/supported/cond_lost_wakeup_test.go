package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestCondLostWakeup is the classic sync.Cond lost-wakeup bug: the waiter calls
// c.Wait() without the mandatory predicate loop, and the signaler may run its
// c.Signal() BEFORE the waiter has parked. A Signal with no waiter is dropped,
// so the waiter then blocks forever. weave explores the "signal before wait"
// ordering and reports it as a deadlock (all goroutines blocked), listing each
// goroutine's wait object. EXPECTED TO FAIL under -weave.
func TestCondLostWakeup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		c := sync.NewCond(&mu)
		go func() {
			mu.Lock()
			c.Signal() // if this beats the Wait below, it is lost
			mu.Unlock()
		}()
		mu.Lock()
		c.Wait() // no predicate loop: the bug
		mu.Unlock()
	})
}
