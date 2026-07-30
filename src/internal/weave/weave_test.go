// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave_test

import (
	"internal/weave"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// The controller serializes participants: only one runs at a time, so appending
// to a plain slice from several goroutines is safe and the order is
// deterministic. main runs first (it spawns the children parked, then appends 0
// and exits), after which the children run in FIFO order.
func TestSerializedOrder(t *testing.T) {
	want := []int{0, 1, 2, 3}
	for iter := 0; iter < 50; iter++ {
		var order []int
		weave.Run(func() {
			go func() { order = append(order, 1) }()
			go func() { order = append(order, 2) }()
			go func() { order = append(order, 3) }()
			order = append(order, 0)
		})
		if !slices.Equal(order, want) {
			t.Fatalf("iter %d: order = %v, want %v", iter, order, want)
		}
	}
}

// weave.Run uses a single deterministic schedule that prefers the lowest-id
// participant at each scheduling point. main (id 0) is always preferred, so it
// runs to completion before the spawned goroutine (id 1). Trace:
//
//	main: append 20, yield -> main still preferred
//	main: append 21, exit  -> hand to g1
//	g1:   append 10, yield -> g1 only, continues
//	g1:   append 11
func TestYieldInterleaves(t *testing.T) {
	want := []int{20, 21, 10, 11}
	for iter := 0; iter < 50; iter++ {
		var order []int
		weave.Run(func() {
			go func() {
				order = append(order, 10)
				weave.Yield()
				order = append(order, 11)
			}()
			order = append(order, 20)
			weave.Yield()
			order = append(order, 21)
		})
		if !slices.Equal(order, want) {
			t.Fatalf("iter %d: order = %v, want %v", iter, order, want)
		}
	}
}

// A real unbuffered channel rendezvous: the receiver blocks (park_m hands the
// token to the sender), the sender wakes the receiver (ready captures it into
// the controller), and the controller resumes it. No weave.Chan wrapper needed.
func TestRealChannel(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		got := -1
		weave.Run(func() {
			ch := make(chan int)
			go func() { ch <- 42 }()
			got = <-ch
		})
		if got != 42 {
			t.Fatalf("iter %d: got %d, want 42", iter, got)
		}
	}
}

// A real sync.Mutex guarding a counter. Serialized run-to-completion, so the
// result is always 2 with no false failure and no hang.
func TestRealMutex(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		x := 0
		weave.Run(func() {
			var mu sync.Mutex
			inc := func() { mu.Lock(); x++; mu.Unlock() }
			go inc()
			go inc()
		})
		if x != 2 {
			t.Fatalf("iter %d: x = %d, want 2", iter, x)
		}
	}
}

// A real sync.Mutex under contention: g1 holds the lock and yields, g2 then
// blocks acquiring it (semacquire -> park_m -> token handoff), and g1's Unlock
// wakes g2 (semrelease -> ready -> controller). Exercises the real block/wake
// path for mutexes.
func TestRealMutexContended(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		x := 0
		weave.Run(func() {
			var mu sync.Mutex
			go func() {
				mu.Lock()
				weave.Yield() // hold the lock while another goroutine runs
				x++
				mu.Unlock()
			}()
			go func() {
				mu.Lock()
				x++
				mu.Unlock()
			}()
		})
		if x != 2 {
			t.Fatalf("iter %d: x = %d, want 2", iter, x)
		}
	}
}

// Explore enumerates distinct interleavings across runs. A benign model with
// several scheduling points must explore more than one schedule and never
// report a false deadlock.
// Without -weave the x accesses are invisible, so the only transitions are the
// (independent) Yields and goroutine start/exit. DPOR correctly recognizes there
// are no conflicts and explores a single representative schedule.
func TestExploreIndependent(t *testing.T) {
	res := weave.Explore(func() {
		x := 0
		go func() { x++; weave.Yield(); x++ }()
		go func() { x++; weave.Yield(); x++ }()
	})
	if res.Deadlock || res.Failed {
		t.Fatalf("unexpected failure: %+v", res)
	}
	// The x accesses are invisible without -weave, so the only transitions are the
	// independent Yields and goroutine start/exit. DPOR must recognize there is
	// nothing to reverse and explore a single representative schedule; asserting that
	// (not just "no failure") is what makes this a reduction test rather than a
	// liveness one. That one schedule is run up to three times by the reproducibility
	// probe (a clean default schedule is replayed to check the model is stable), so
	// the bound is 3; a second, spurious interleaving would push Runs past it.
	if res.Runs > 3 {
		t.Fatalf("independent operations have no conflict to reverse, so DPOR should explore a "+
			"single schedule (run up to 3x by the reproducibility probe); explored %d", res.Runs)
	}
	t.Logf("independent ops: DPOR explored %d schedule(s)", res.Runs)
}

// TestGCPreemptNoSpuriousDeadlock guards the park_m block/wake fix: a
// participant must be marked weaveBlocked *before* its park becomes observable
// to a waker, so a concurrent out-of-bubble GC that preempts and resumes a
// blocking/waking participant can never make it OS-runnable behind the
// controller's back (which used to surface as a spurious deadlock). We hammer
// runtime.GC() from outside the bubble while repeatedly exploring a model that
// exercises the real mutex and channel block/wake paths. The result must always
// be clean, and because DPOR is deterministic the schedule count must be
// identical every iteration — a leaked block/wake race shows up as either a
// spurious deadlock or a varying schedule count.
func TestGCPreemptNoSpuriousDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("GC-preempt stress; skipped in -short")
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
	time.Sleep(2 * time.Millisecond) // let the GC hammer threads spin up

	model := func() {
		var mu sync.Mutex
		x := 0
		done := make(chan int, 2)
		inc := func() {
			mu.Lock()
			x++
			mu.Unlock()
			done <- 1
		}
		go inc()
		go inc()
		<-done
		<-done
		if x != 2 {
			panic("lost update")
		}
	}

	first := weave.Explore(model)
	if first.Deadlock || first.Failed {
		close(stop)
		wg.Wait()
		t.Fatalf("iter 0: spurious failure under GC pressure: %+v", first)
	}
	for iter := 1; iter < 20; iter++ {
		res := weave.Explore(model)
		if res.Deadlock || res.Failed {
			close(stop)
			wg.Wait()
			t.Fatalf("iter %d: spurious failure under GC pressure: %+v", iter, res)
		}
		if res.Runs != first.Runs {
			close(stop)
			wg.Wait()
			t.Fatalf("iter %d: nondeterministic schedule count %d (want %d) — block/wake race leaked",
				iter, res.Runs, first.Runs)
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("no spurious deadlock across 20 explorations under GC hammer; %d schedules each", first.Runs)
}

// The AB/BA mutex deadlock is exercised in internal_test.go via exhaustive
// exploration: DPOR does not yet record mutex operations, so it cannot reduce
// (or soundly reason about) sync-only conflicts. See task: sync-op recording.

// Note: the read-modify-write lost update is now exercised soundly under
// compiler instrumentation in instr_test.go (TestAutoLostUpdate), where the x
// accesses are observable to DPOR. A Yield-only version is not a valid DPOR test
// because the conflicting accesses would be invisible.
