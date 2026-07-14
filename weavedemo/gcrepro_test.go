package weavedemo

import (
	"runtime"
	"sync"
	"testing"
	"testing/weave"
	"time"
)

// spin does a long pure-register computation with no function calls, so the
// only way to preempt it is an asynchronous signal (it has no cooperative
// safepoint). Returned to defeat dead-code elimination.
//
//go:noinline
func spin(n int) uint64 {
	var x uint64 = 1
	for i := 0; i < n; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 7
	}
	return x
}

var spinSink uint64

// TestGCPreemptHolder stresses the interaction between GC stack-scan preemption
// and weave's run token. A participant holds the token while spinning long
// enough that a concurrent, out-of-bubble runtime.GC() will suspendG it in
// _Grunning and drive it through _Gpreempted -> ready. If weave loses the token
// on that resume, the run hangs.
func TestGCPreemptHolder(t *testing.T) {
	if builtWithWeave {
		t.Skip("prohibitively slow under -weave memory instrumentation; this stress test exercises the run token under GC preemption, not data races, so run it without -weave")
	}
	iters := 200
	if testing.Short() {
		iters = 20
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					runtime.GC()
				}
			}
		}()
	}
	// Give the GC hammer threads a head start.
	time.Sleep(2 * time.Millisecond)

	for iter := 0; iter < iters; iter++ {
		weave.Test(t, func() {
			var mu sync.Mutex
			x := 0
			inc := func() {
				mu.Lock()
				spinSink = spin(200000) // hold the token, spinning, GC can catch it
				x = x + 1
				mu.Unlock()
			}
			go inc()
			go inc()
			go inc()
			weave.Wait()
			if x != 3 {
				panic("lost update under GC")
			}
		})
	}
	close(stop)
	wg.Wait()
}
