package supported

import (
	"testing"
	"testing/synctest"
)

// TestMultiProducerAppendRace is an unsynchronized multi-producer append: two
// goroutines append to the same slice header with no lock. append reads the
// current slice, writes the element, and reassigns s; interleaved, both can read
// the same empty s and each produce a length-1 slice, so one element is lost
// (final len 1, not 2). weave turns the slice-header read/write into scheduling
// points and finds the lost append. EXPECTED TO FAIL under -weave. The fix is a
// mutex around the append (or a channel to fan the values into one owner).
func TestMultiProducerAppendRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s []int
		go func() { s = append(s, 1) }()
		go func() { s = append(s, 2) }()
		synctest.Wait()
		if len(s) != 2 {
			t.Fatalf("lost append: len=%d", len(s))
		}
	})
}
