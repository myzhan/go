package supported

import (
	"testing"
	"testing/synctest"
)

// TestSliceIndexOutOfRange is a slice-header data race whose symptom is a
// bounds-check panic, not a wrong number or a deadlock. One goroutine reslices s
// down to length 1 while another indexes s[2]. The two share the slice header
// (ptr/len/cap) with no synchronization: in the interleaving where the truncation
// is observed first, the index expression's bounds check sees length 1 and panics
// "index out of range [2] with length 1". weave turns the slice-header read/write
// into scheduling points, finds that interleaving, and CAPTURES the panic.
// EXPECTED TO FAIL under -weave; PASSES in a single synctest schedule. -race also
// flags the unsynchronized access; the fix is a lock (or not sharing the header).
func TestSliceIndexOutOfRange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := []int{10, 20, 30}
		got := 0
		go func() { s = s[:1] }()  // truncate to length 1
		go func() { got = s[2] }() // fixed-index read: out of range once truncated
		synctest.Wait()
		_ = got
	})
}
