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
// weave finds it twice over: since ADR D18 the typed sync/atomic operations are
// themselves scheduling points, and independently the `n.next = head.Load()`
// assignment writes a PLAIN field, which is also a scheduling point under -weave.
// So this was explorable even back when atomics were a blind spot — the lesson
// being that a realistic lock-free structure threads plain memory through its
// operations, and that alone keeps it reachable. EXPECTED TO FAIL under -weave.
// Compare atomic_lost_update_test.go, which is PURELY atomic and so needed D18 to
// be found at all.
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
