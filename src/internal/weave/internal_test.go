// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"sync"
	"testing"
)

// The classic AB/BA lock-ordering deadlock in real sync.Mutexes. DPOR does not
// yet record mutex operations, so this uses exhaustive exploration; the Yield
// between the two Lock calls lets both goroutines take their first lock before
// the second, reaching the deadlock.
func TestExhaustiveFindsDeadlock(t *testing.T) {
	res := exploreExhaustive(func() {
		var a, b sync.Mutex
		go func() {
			a.Lock()
			Yield()
			b.Lock()
			b.Unlock()
			a.Unlock()
		}()
		go func() {
			b.Lock()
			Yield()
			a.Lock()
			a.Unlock()
			b.Unlock()
		}()
	})
	if !res.Deadlock {
		t.Fatalf("expected to find AB/BA deadlock; explored %d schedules", res.Runs)
	}
	t.Logf("found deadlock after exploring %d schedule(s)", res.Runs)
}

// Mutex operations are now recorded as transitions, so DPOR finds the AB/BA
// lock-ordering deadlock directly — no explicit Yield needed, and with reduction.
func TestMutexDPORFindsDeadlock(t *testing.T) {
	res := Explore(func() {
		var a, b sync.Mutex
		go func() {
			a.Lock()
			b.Lock()
			b.Unlock()
			a.Unlock()
		}()
		go func() {
			b.Lock()
			a.Lock()
			a.Unlock()
			b.Unlock()
		}()
	})
	if !res.Deadlock {
		t.Fatalf("expected DPOR to find AB/BA mutex deadlock; explored %d schedules", res.Runs)
	}
	t.Logf("DPOR found mutex deadlock after %d schedule(s); seed %q", res.Runs, res.Seed)
}

// Channel operations are recorded as transitions, so DPOR reasons about them
// soundly with no compiler instrumentation. Two sends to the same buffered
// channel race; the order determines which value is received first. DPOR must
// find the order-dependent failure and reach the same outcome as exhaustive
// exploration while running no more schedules.
func TestChannelDPOREquivalence(t *testing.T) {
	model := func() {
		ch := make(chan int, 2)
		done := make(chan bool, 2)
		go func() { ch <- 1; done <- true }()
		go func() { ch <- 2; done <- true }()
		<-done
		<-done
		a := <-ch
		_ = <-ch
		if a != 1 { // order-dependent: fails when the second goroutine sends first
			panic("order-dependent receive")
		}
	}
	dpor := Explore(model)
	exh := exploreExhaustive(model)
	if dpor.Failed != exh.Failed || dpor.Deadlock != exh.Deadlock {
		t.Fatalf("DPOR/exhaustive disagree: dpor=%+v exhaustive=%+v", dpor, exh)
	}
	if !exh.Failed {
		t.Fatalf("expected an order-dependent failure")
	}
	if dpor.Runs > exh.Runs {
		t.Fatalf("DPOR explored more than exhaustive: dpor=%d exhaustive=%d", dpor.Runs, exh.Runs)
	}
	t.Logf("channel conflicts: DPOR %d schedules vs exhaustive %d", dpor.Runs, exh.Runs)
}

// A discovered failure carries a seed; replaying it reproduces the same
// interleaving (participant/op sequence) every time. (Addresses differ across
// runs since the heap layout changes, so only wid+op are compared.)
func TestReplayReproduces(t *testing.T) {
	model := func() {
		ch := make(chan int, 2)
		done := make(chan bool, 2)
		go func() { ch <- 1; done <- true }()
		go func() { ch <- 2; done <- true }()
		<-done
		<-done
		a := <-ch
		_ = <-ch
		if a != 1 {
			panic("order-dependent receive")
		}
	}
	res := Explore(model)
	if !res.Failed || res.Seed == "" {
		t.Fatalf("expected a failure with a seed; got %+v", res)
	}
	for i := 0; i < 3; i++ {
		rep := Replay(res.Seed, model)
		if !rep.Failed {
			t.Fatalf("replay %d: seed %q did not reproduce the failure", i, res.Seed)
		}
		if !sameSchedule(rep.Trace, res.Trace) {
			t.Fatalf("replay %d: interleaving differs\norig: %v\nrep:  %v", i, res.Trace, rep.Trace)
		}
	}
	t.Logf("seed %q reproduces the failing interleaving deterministically", res.Seed)
}

// Map iteration order is determinized inside a controlled bubble, so a model
// whose behavior depends on it produces the same outcome every run.
func TestMapIterationDeterministic(t *testing.T) {
	model := func() {
		m := map[int]int{}
		for i := 0; i < 16; i++ {
			m[i] = i
		}
		first := -1
		for k := range m {
			first = k
			break
		}
		if first == 3 { // arbitrary; the point is it's the same every run
			panic("map-order-dependent")
		}
	}
	a := Explore(model)
	b := Explore(model)
	if a.Failed != b.Failed {
		t.Fatalf("map iteration nondeterministic across runs: %v vs %v", a.Failed, b.Failed)
	}
	t.Logf("map iteration deterministic across runs (failed=%v)", a.Failed)
}

// A correct model with a large state space is explored fully by default, but a
// tiny budget stops early and marks the result Truncated instead of reporting a
// clean pass.
func TestBudgetTruncates(t *testing.T) {
	model := func() {
		ch := make(chan int, 2)
		done := make(chan bool, 2)
		go func() { ch <- 1; done <- true }()
		go func() { ch <- 2; done <- true }()
		<-done
		<-done
		a := <-ch
		b := <-ch
		if a+b != 3 { // always true: {1,2} in some order — a correct model
			panic("impossible")
		}
	}
	full := Explore(model)
	if full.Truncated {
		t.Fatalf("unbounded Explore should not truncate; got %+v", full)
	}
	if full.Runs <= 3 {
		t.Fatalf("model too small to exercise budget: full run explored %d schedules", full.Runs)
	}
	res := ExploreBudget(model, 3)
	if !res.Truncated {
		t.Fatalf("expected truncation under budget 3; explored %d, truncated=%v", res.Runs, res.Truncated)
	}
	if res.Failed || res.Deadlock {
		t.Fatalf("model is correct; unexpected failure: %+v", res)
	}
	if res.Runs > 3 {
		t.Fatalf("budget 3 exceeded: ran %d schedules", res.Runs)
	}
	t.Logf("budget truncation: ran %d schedule(s), reason %q (full = %d)", res.Runs, res.TruncatedReason, full.Runs)
}

// Context bounding: the lost update needs a preemption in the middle of a
// read-modify-write (switch away from a goroutine that is still runnable at its
// Yield). With 0 preemptions allowed, goroutines run to completion in turn and
// the bug is unreachable; allowing preemptions surfaces it. The bound also cuts
// the number of schedules explored.
func TestPreemptionBound(t *testing.T) {
	model := func() {
		x := 0
		done := make(chan bool, 2)
		inc := func() {
			tmp := x
			Yield() // split the read-modify-write with a scheduling point
			x = tmp + 1
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if x != 2 {
			panic("lost update")
		}
	}

	b0 := ExploreBounded(model, DefaultMaxSchedules, 0, 0)
	if b0.Failed || b0.Deadlock {
		t.Fatalf("with 0 preemptions the lost update is unreachable; got %+v", b0)
	}
	if b0.Truncated {
		t.Fatalf("a preemption bound should not mark the result truncated; got %+v", b0)
	}

	b2 := ExploreBounded(model, DefaultMaxSchedules, 2, 0)
	if !b2.Failed {
		t.Fatalf("with preemptions allowed, expected to find the lost update; got %+v", b2)
	}

	full := Explore(model)
	if !full.Failed {
		t.Fatalf("unbounded exploration should also find the lost update")
	}
	t.Logf("preemption bound: c=0 clean (%d schedules), c=2 found bug (%d), unbounded (%d)",
		b0.Runs, b2.Runs, full.Runs)
}

// The newly recorded primitives (WaitGroup.Add/Done, sync.Once, sync.Cond) are
// explorable end to end: correct uses of them are driven through every
// interleaving without a spurious failure or deadlock.
func TestSyncPrimitivesExplorable(t *testing.T) {
	wg := Explore(func() {
		var wg sync.WaitGroup
		done := make(chan int, 2)
		wg.Add(2)
		go func() { done <- 1; wg.Done() }()
		go func() { done <- 2; wg.Done() }()
		wg.Wait()
		<-done
		<-done
	})
	if wg.Failed || wg.Deadlock {
		t.Fatalf("WaitGroup model should be clean; got %+v", wg)
	}

	once := Explore(func() {
		var once sync.Once
		n := 0
		f := func() { n++ }
		done := make(chan bool, 2)
		go func() { once.Do(f); done <- true }()
		go func() { once.Do(f); done <- true }()
		<-done
		<-done
		if n != 1 {
			panic("Once ran f more than once")
		}
	})
	if once.Failed || once.Deadlock {
		t.Fatalf("Once model should be clean; got %+v", once)
	}

	cond := Explore(func() {
		var mu sync.Mutex
		c := sync.NewCond(&mu)
		ready := false
		go func() {
			mu.Lock()
			ready = true
			c.Signal()
			mu.Unlock()
		}()
		mu.Lock()
		for !ready {
			c.Wait()
		}
		mu.Unlock()
	})
	if cond.Failed || cond.Deadlock {
		t.Fatalf("Cond model should be clean; got %+v", cond)
	}
	t.Logf("sync primitives explorable: wg=%d once=%d cond=%d schedules", wg.Runs, once.Runs, cond.Runs)
}

func sameSchedule(a, b []Step) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Wid != b[i].Wid || a[i].Op != b[i].Op {
			return false
		}
	}
	return true
}
