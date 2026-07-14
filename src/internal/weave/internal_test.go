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

// Cond.Broadcast wakes every waiter, not just one. Two goroutines Wait on the
// cond; the main participant sets the predicate and Broadcasts. Every
// interleaving must wake both waiters (woke == 2) with no lost wakeup and no
// spurious deadlock. (Cond ops are recorded without -weave, so this needs no
// build flag; joining via weave.Wait keeps the state space small.)
func TestCondBroadcast(t *testing.T) {
	res := Explore(func() {
		var mu sync.Mutex
		c := sync.NewCond(&mu)
		ready := false
		woke := 0
		waiter := func() {
			mu.Lock()
			for !ready {
				c.Wait()
			}
			woke++
			mu.Unlock()
		}
		go waiter()
		go waiter()
		mu.Lock()
		ready = true
		c.Broadcast()
		mu.Unlock()
		Wait()
		if woke != 2 {
			panic("Broadcast did not wake both waiters")
		}
	})
	if res.Failed || res.Deadlock {
		t.Fatalf("Cond.Broadcast model should be clean; got %+v", res)
	}
	t.Logf("Cond.Broadcast wakes all waiters cleanly across %d schedule(s)", res.Runs)
}

// Select-case enumeration: with both cases of a select ready, the explorer must
// try each one. The model panics only when the second case fires, so a working
// enumeration finds the failure, and the seed (which encodes the select choice)
// reproduces it. Channel ops are recorded without -weave, so this needs no flag.
func TestSelectEnumeration(t *testing.T) {
	model := func() {
		a := make(chan int, 1)
		b := make(chan int, 1)
		a <- 1
		b <- 2
		var got int
		select {
		case got = <-a:
		case got = <-b:
		}
		if got == 2 {
			panic("selected b")
		}
	}
	res := Explore(model)
	if !res.Failed {
		t.Fatalf("expected enumeration to reach the b case (panic); got %+v", res)
	}
	rep := Replay(res.Seed, model)
	if !rep.Failed {
		t.Fatalf("replay of seed %q did not reproduce the select-dependent failure", res.Seed)
	}
	t.Logf("select enumeration reached the b case; seed %q reproduces", res.Seed)
}

// A two-case blocking select competes with a concurrent send on one of its
// channels. The "other" case is always ready (pre-buffered), while the "ch" case
// is ready only once the concurrent sender has run. If the select is ordered
// before the send it can only take "other" (99); if the sender is ordered first,
// ch becomes ready and the select may take it (1). Both outcomes must be
// explored, which requires DPOR to treat the select transition as dependent on
// the concurrent send (the select records addr 0, so this dependency is matched
// address-agnostically in conflict). A single-case select with a default is
// compiled to a non-blocking selectnbrecv and would not exercise selectgo, so
// two real cases with no default are used. Channel ops are recorded without
// -weave, so this needs no flag.
func TestSelectVsConcurrentSend(t *testing.T) {
	model := func(record func(int)) func() {
		return func() {
			ch := make(chan int, 1)
			other := make(chan int, 1)
			go func() { ch <- 1 }() // buffered: never blocks
			other <- 99             // "other" case is always ready
			var result int
			select {
			case v := <-ch:
				result = v
			case v := <-other:
				result = v
			}
			Wait()
			record(result)
		}
	}
	states := map[int]bool{}
	Explore(model(func(r int) { states[r] = true }))
	if !states[1] || !states[99] {
		t.Fatalf("select-vs-send: DPOR reached %v, want both outcomes {1, 99}", states)
	}
	t.Logf("select-vs-send: DPOR reached both orderings %v", states)
}

// A non-blocking select (single case with a default, compiled to selectnbrecv →
// chanrecv with block=false) competes with a concurrent send: whether the
// receive case or the default fires depends on the order of the poll relative to
// the send. Both outcomes must be explored, which requires the non-blocking
// channel operation to be a scheduling point (recorded as opChanRecvNB, ordered
// before the fast-path check in chanrecv). Channel ops are recorded without
// -weave, so this needs no flag.
func TestNonBlockingSelectVsSend(t *testing.T) {
	model := func(record func(int)) func() {
		return func() {
			ch := make(chan int, 1)
			go func() { ch <- 1 }() // buffered: never blocks
			var result int
			select {
			case v := <-ch:
				result = v // the send was ordered before the poll
			default:
				result = -1 // the poll ran first; channel still empty
			}
			Wait()
			record(result)
		}
	}
	states := map[int]bool{}
	Explore(model(func(r int) { states[r] = true }))
	if !states[1] || !states[-1] {
		t.Fatalf("non-blocking select-vs-send: DPOR reached %v, want both outcomes {1, -1}", states)
	}
	t.Logf("non-blocking select-vs-send: DPOR reached both orderings %v", states)
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
