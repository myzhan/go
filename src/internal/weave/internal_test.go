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
