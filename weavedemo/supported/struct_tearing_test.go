package supported

import (
	"testing"
	"testing/synctest"
)

// TestStructTearing is an unsynchronized whole-struct publication whose invariant
// is x == y: a writer assigns point{5, 5} while a reader copies the struct. A
// multi-word struct assignment is not atomic, so the reader can observe a TORN
// struct — one field updated and the other not (e.g. {5, 0}). -race also flags
// the unsynchronized access.
//
// weave now FINDS it. Under -weave the compiler stores a pointer-free multi-field
// struct field-by-field with a scheduling point before each field store (see
// .claude/weave/design.md D19), so weave can interleave the reader between the
// two field writes and observe {5, 0}. EXPECTED TO FAIL under -weave; PASSES in a
// single serial synctest schedule. The fix is a mutex or atomic pointer swap so
// the whole value is published atomically.
func TestStructTearing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type point struct{ x, y int }
		var p point // invariant: x == y (the two fields are updated together)
		go func() { p = point{x: 5, y: 5} }()
		go func() {
			q := p // whole-struct read racing the write
			if q.x != q.y {
				panic("torn struct read: fields updated non-atomically")
			}
		}()
		synctest.Wait()
	})
}
