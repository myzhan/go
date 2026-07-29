package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestWaitGroupAddInGoroutine is the classic WaitGroup misuse: wg.Add(1) is
// called INSIDE the spawned goroutine instead of before the `go` statement. In
// the interleaving where wg.Wait() runs before the goroutine reaches Add, the
// counter is still 0, so Wait returns immediately and the caller proceeds before
// the work is done. weave finds that interleaving (Add/Wait are scheduling
// points, and the `done` flag's read/write are too) and reports the premature
// return. EXPECTED TO FAIL under -weave. The fix is to call wg.Add(1) before
// `go`, so the count is established before anyone can Wait.
func TestWaitGroupAddInGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		done := false
		go func() {
			wg.Add(1) // WRONG: races Wait; should be before the go statement
			done = true
			wg.Done()
		}()
		wg.Wait()
		if !done {
			t.Fatal("Wait returned before the work was done")
		}
	})
}
