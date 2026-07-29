package supported

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// TestAtomicLostUpdate is a non-atomic read-modify-write built out of
// atomic.Load + atomic.Store. The composite (Load, +1, Store) is NOT atomic, so
// two goroutines can both Load 0 and both Store 1 — a lost update leaving x == 1.
//
// weave now FINDS it. sync/atomic's typed operations are scheduling points (the
// atomic methods call runtime.weaveSchedPoint, gated by weaveGloballyActive, just
// like sync.Mutex), so weave can preempt between the Load and the Store and
// explore the interleaving where both read 0. EXPECTED TO FAIL under -weave;
// PASSES in a single serial synctest schedule.
//
// Note -race does NOT catch this: every individual access is atomic, so there is
// no data race — only a logical atomicity violation. The correct fix is
// x.Add(1) (or a CompareAndSwap loop), which weave verifies as correct.
func TestAtomicLostUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var x atomic.Int64
		go func() { x.Store(x.Load() + 1) }()
		go func() { x.Store(x.Load() + 1) }()
		synctest.Wait()
		if x.Load() != 2 {
			t.Fatalf("lost update: x=%d", x.Load())
		}
	})
}
