package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// TestDiningPhilosophers is the classic multi-party circular-wait deadlock: 3
// philosophers each grab their LEFT fork then their RIGHT fork (forks are
// mutexes), all in the same order. In the interleaving where every philosopher
// holds their left fork and then waits for their right (held by the next
// philosopher), no one can proceed — a 3-way cycle. This is distinct from the
// 2-way AB/BA deadlock: the wait cycle spans three goroutines. weave explores the
// interleavings and reports the deadlock. EXPECTED TO FAIL under -weave; PASSES in
// a single serial synctest schedule. The fix is to break the symmetry (e.g. one
// philosopher grabs right-then-left, or acquire forks in a global order).
func TestDiningPhilosophers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var forks [3]sync.Mutex
		var wg sync.WaitGroup
		for i := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				left, right := i, (i+1)%3
				forks[left].Lock()
				forks[right].Lock()
				// eat (both forks held)
				forks[right].Unlock()
				forks[left].Unlock()
			}()
		}
		wg.Wait()
	})
}
