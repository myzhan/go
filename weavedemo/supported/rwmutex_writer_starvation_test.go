package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestRWMutexWriterStarvation is a self-deadlock from re-entrant read locking.
// Go's sync.RWMutex is NOT re-entrant and gives writers priority: once a writer
// is blocked in Lock(), any new RLock() blocks behind it to prevent writer
// starvation. So a goroutine that holds an RLock and then takes a second RLock
// deadlocks in the interleaving where a writer queued in between — the second
// RLock waits for the writer, the writer waits for the first RLock to release.
// weave finds that interleaving and reports the deadlock. EXPECTED TO FAIL under
// -weave. (Serialized, the writer runs entirely before or after, so it PASSES
// without -weave.)
func TestRWMutexWriterStarvation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var rw sync.RWMutex
		config := 0
		go func() {
			rw.Lock()
			config = 1
			rw.Unlock()
		}()
		rw.RLock()
		_ = config
		rw.RLock() // re-entrant: blocks behind a queued writer -> deadlock
		_ = config
		rw.RUnlock()
		rw.RUnlock()
	})
}
