package supported

import (
	"testing"
	"testing/synctest"
)

// TestLostUpdate is the simplest lost update there is: two goroutines run
// x = x + 1 with no synchronization at all. The read and the write are separate
// steps, so an interleaving where both read 0 leaves x == 1.
//
// Under -weave the compiler turns the read and the write of x into scheduling
// points, so weave finds that interleaving. EXPECTED TO FAIL under -weave.
// The fix is a mutex around the increment (see correct_sync_basics_test.go) or
// atomic.Int64.Add.
func TestLostUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := 0
		go func() { x = x + 1 }()
		go func() { x = x + 1 }()
		synctest.Wait() // wait for both goroutines to finish before asserting
		if x != 2 {
			t.Fatal("lost update")
		}
	})
}
