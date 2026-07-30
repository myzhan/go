package unsupported

import (
	"testing"
	"testing/synctest"
)

// TestMapConcurrentWrite writes to the same map from two goroutines with no
// synchronization — a data race that the Go runtime detects at real concurrency
// as a fatal "concurrent map writes", and that -race flags.
//
// weave MISSES it, a sibling of the sync/atomic gap: a map assignment is a
// runtime call (mapassign) with no scheduling point inside it, so weave never
// interleaves the two writes mid-operation — it explores them as atomic units
// and reports no failure. (It is also safe here precisely because weave
// serializes participants, so the fatal race condition is never physically
// triggered.) Instrumenting map operations as scheduling points would close this
// gap; until then this stays a known false negative. What DOES catch it is
// `-race`; the fix is a mutex or sync.Map.
func TestMapConcurrentWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := map[int]int{}
		go func() { m[1] = 1 }()
		go func() { m[2] = 2 }()
		synctest.Wait()
		if len(m) != 2 {
			t.Fatalf("lost map write: len=%d", len(m))
		}
	})
}
