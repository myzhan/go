package unsupported

import (
	"testing"
	"testing/synctest"
)

// TestWeakMemoryPublication is an unsynchronized publication bug: the writer sets
// data then a ready flag; the reader, seeing ready, expects data to be visible.
// On real hardware / under the Go memory model this is a data race, and the
// reader can observe ready==true while data is still 0 (store-store reordering
// or stale cache) — a torn publication.
//
// weave MISSES it because it models SEQUENTIAL CONSISTENCY and respects each
// goroutine's program order: within the writer, `data = 42` always precedes
// `ready = true`, and weave never reorders those stores or introduces stale
// reads. So no explored interleaving can show ready-without-data; weave PASSES.
// Weak-memory (store buffering / read-from enumeration) is explicitly out of the
// current model. What DOES flag this is `-race` (it is a genuine data race on
// two unsynchronized plain variables); the fix is a real happens-before edge
// (mutex/channel/atomic) between publish and consume.
func TestWeakMemoryPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		data := 0
		ready := false
		go func() {
			data = 42
			ready = true
		}()
		go func() {
			if ready && data != 42 {
				panic("torn publication: ready without data")
			}
		}()
		synctest.Wait()
	})
}
