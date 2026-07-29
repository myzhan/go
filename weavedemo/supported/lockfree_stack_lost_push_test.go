package supported

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// TestLockFreeStackLostPush is a broken lock-free stack: push does
// head.Store(n) where a correct implementation would loop on CompareAndSwap. Two
// concurrent pushes can both read the same head and then overwrite each other,
// losing one node (final depth 1, not 2).
//
// Notably weave FINDS this even though the head Load/Store are sync/atomic ops
// that weave does not instrument: the `n.next = head.Load()` assignment writes a
// PLAIN field, and that write IS a scheduling point, so weave can preempt
// between reading head and storing it. The lesson is the flip side of
// ../unsupported/atomic_lost_update: weave's atomic gap only hides bugs in code
// that is PURELY atomic with no plain-memory access between the operations; a
// realistic lock-free structure threads plain field writes through its
// operations, and those keep it explorable. EXPECTED TO FAIL under -weave.
func TestLockFreeStackLostPush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type node struct {
			v    int
			next *node
		}
		var head atomic.Pointer[node]
		push := func(v int) {
			n := &node{v: v}
			n.next = head.Load() // plain field write — a scheduling point
			head.Store(n)        // BUG: should be a CAS loop; a concurrent push is lost
		}
		go push(1)
		go push(2)
		synctest.Wait()
		depth := 0
		for n := head.Load(); n != nil; n = n.next {
			depth++
		}
		if depth != 2 {
			t.Fatalf("lost push: depth=%d", depth)
		}
	})
}
