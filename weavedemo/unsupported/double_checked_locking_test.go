package unsupported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestDoubleCheckedLocking is the classic double-checked-locking hazard. The
// first `instance == nil` check is unsynchronized. On real hardware the store
// that publishes the pointer can become visible before the writes that
// initialize the object's fields, so a reader taking the fast path can observe a
// non-nil pointer to a half-constructed object (val still 0) — a torn read.
//
// weave MISSES it: it models SEQUENTIAL CONSISTENCY and each goroutine's program
// order, so `instance = &cfg{val: 42}` always makes the field write precede (and
// be visible with) the pointer publication. No explored interleaving yields a
// non-nil pointer with val != 42, so weave PASSES. This is the same weak-memory
// blind spot as ../unsupported/weak_memory_publication, in its most famous
// real-world guise. What DOES flag it is `-race` (the fast-path read of instance
// is a genuine data race); the fix is sync.Once or an atomic pointer.
func TestDoubleCheckedLocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type cfg struct{ val int }
		var mu sync.Mutex
		var instance *cfg
		get := func() *cfg {
			if instance == nil { // unsynchronized fast-path check
				mu.Lock()
				if instance == nil {
					instance = &cfg{val: 42} // publishes pointer + field write
				}
				mu.Unlock()
			}
			return instance
		}
		go func() { _ = get() }()
		go func() {
			c := get()
			if c != nil && c.val != 42 { // would fire on a torn publication
				panic("DCL saw a half-constructed object")
			}
		}()
		synctest.Wait()
	})
}
