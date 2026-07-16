// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Fake-clock advancement: a participant that sleeps must be released by the
// controller advancing the bubble's fake clock, not deadlock. If the clock did
// not advance, the receive below would block forever and Run would panic with a
// deadlock.
func TestFakeClockSleepCompletes(t *testing.T) {
	Run(func() {
		ch := make(chan bool, 1)
		go func() {
			time.Sleep(time.Second)
			ch <- true
		}()
		<-ch
	})
	t.Log("time.Sleep released by fake-clock advancement")
}

// Timers fire in deadline order: the 1s sleeper wakes before the 2s sleeper.
func TestFakeClockDeadlineOrder(t *testing.T) {
	Run(func() {
		ch := make(chan int, 2)
		go func() { time.Sleep(2 * time.Second); ch <- 2 }()
		go func() { time.Sleep(1 * time.Second); ch <- 1 }()
		a := <-ch
		b := <-ch
		if a != 1 || b != 2 {
			panic("timers fired out of deadline order")
		}
	})
	t.Log("timers fired in deadline order")
}

// A select with a time.After timeout: when the worker is slower than the
// timeout, fake-clock advancement fires the earlier (timeout) timer, so the
// timeout branch is taken. Without clock advancement this select would deadlock.
func TestFakeClockTimeoutFires(t *testing.T) {
	Run(func() {
		result := make(chan int, 1)
		go func() { time.Sleep(2 * time.Second); result <- 42 }()
		var outcome string
		select {
		case <-result:
			outcome = "result"
		case <-time.After(time.Second):
			outcome = "timeout"
		}
		if outcome != "timeout" {
			panic("expected the 1s timeout to fire before the 2s worker")
		}
	})
	t.Log("time.After timeout reachable under fake-clock advancement")
}

// context.WithTimeout is timer-backed; it must also fire under weave.
func TestFakeClockContextTimeout(t *testing.T) {
	Run(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		<-ctx.Done()
		if ctx.Err() != context.DeadlineExceeded {
			panic("context did not time out")
		}
	})
	t.Log("context.WithTimeout fires under fake-clock advancement")
}

// A genuine deadlock with no pending timer must still be reported as a deadlock,
// not mistaken for a clock-advance opportunity.
func TestFakeClockNoTimerStillDeadlock(t *testing.T) {
	res := Explore(func() {
		ch := make(chan int) // unbuffered, no sender
		<-ch
	})
	if !res.Deadlock {
		t.Fatalf("expected deadlock with no timer, got %+v", res)
	}
	t.Log("no-timer block still reported as deadlock")
}

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

// Mutex.TryLock's success depends on whether another goroutine already holds the
// lock, which is schedule-dependent. Both outcomes must be explored, which
// requires TryLock to be a scheduling point (regression for the missing TryLock
// hook). Mutex ops are recorded without -weave, so this needs no flag.
func TestTryLockExplored(t *testing.T) {
	model := func(record func(bool)) func() {
		return func() {
			var mu sync.Mutex
			go func() { mu.Lock(); Yield(); mu.Unlock() }()
			ok := mu.TryLock()
			if ok {
				mu.Unlock()
			}
			Wait()
			record(ok)
		}
	}
	states := map[bool]bool{}
	Explore(model(func(ok bool) { states[ok] = true }))
	if !states[true] || !states[false] {
		t.Fatalf("TryLock: DPOR reached %v, want both success and failure", states)
	}
	t.Logf("TryLock: DPOR reached both outcomes %v", states)
}

// A model with exactly one schedule, explored under a budget of 1, is fully
// covered and must not be reported Truncated (regression for the budget
// off-by-one that truncated before checking whether a next schedule remained).
func TestBudgetExactCoverageNotTruncated(t *testing.T) {
	model := func() { x := 0; _ = x }
	res := ExploreBudget(model, 1)
	if res.Truncated {
		t.Fatalf("single-schedule model under budget 1 wrongly Truncated: %+v", res)
	}
	if res.Runs != 1 {
		t.Fatalf("expected exactly 1 run, got %d", res.Runs)
	}
	t.Logf("single-schedule model under budget 1: complete, not truncated")
}

// A single schedule with more scheduling points than the trace buffer must be
// reported Truncated, not panic with slice-out-of-range (regression for slicing
// the trace by steps before checking the capacity outcome).
func TestTraceOverflowTruncates(t *testing.T) {
	model := func() {
		for i := 0; i < traceCap+2; i++ {
			Yield()
		}
	}
	res := ExploreBudget(model, 10)
	if !res.Truncated {
		t.Fatalf("expected Truncated on trace overflow, got %+v", res)
	}
	// Replay builds the trace with the same slicing and must not panic on an
	// overflowing run either; an empty seed replays under the default policy,
	// which overflows the same way.
	rep := Replay("", model)
	if !rep.Truncated {
		t.Fatalf("expected Replay to report Truncated on trace overflow, got %+v", rep)
	}
	t.Logf("trace overflow correctly Truncated (Explore + Replay): %s", res.TruncatedReason)
}

// Spawning more goroutines than the controller's runnable capacity must abort
// the schedule as Truncated (capacity), not crash the whole test process with a
// runtime fatal error (regression for the throw in weavePush).
func TestTooManyGoroutinesTruncates(t *testing.T) {
	model := func() {
		for i := 0; i < 300; i++ {
			go func() {}()
		}
		Wait()
	}
	res := Explore(model)
	if !res.Truncated {
		t.Fatalf("expected Truncated on runnable-capacity overflow, got %+v", res)
	}
	// Run must propagate the capacity overflow (panic), not silently return
	// success as if the model had been fully explored.
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("Run should panic on capacity overflow, not return success")
			}
		}()
		Run(model)
	}()
	t.Logf("capacity overflow correctly Truncated (Explore) and propagated (Run): %s", res.TruncatedReason)
}

// A select waiter (G) is woken through one case (chB), leaving a stale, already
// claimed sudog on the other channel (chA). When another participant then runs a
// select whose chA case would pair with that stale sudog, the readiness peek must
// not treat it as ready: the real, buffered chC case is available, so the model
// always completes. Without excluding stale sudogs the select commits to chA,
// fails, parks, and spuriously reports a deadlock. No -weave needed.
func TestSelectStaleSudogNoFalseDeadlock(t *testing.T) {
	model := func() {
		chA := make(chan int)
		chB := make(chan int)
		chC := make(chan int, 1)
		go func() {
			select {
			case <-chA:
			case <-chB:
			}
		}()
		chC <- 7 // make the chC case genuinely ready
		chB <- 1 // wake G via chB (its only sender); G's chA sudog goes stale
		select {
		case chA <- 2: // a stale G receiver may still linger on chA.recvq
		case <-chC: // really ready (buffered)
		}
		Wait()
	}
	res := Explore(model)
	if res.Deadlock {
		t.Fatalf("spurious deadlock from a stale select sudog; seed %q trace=%+v", res.Seed, res.Trace)
	}
	if res.Truncated {
		t.Fatalf("unexpected truncation: %s", res.TruncatedReason)
	}
	t.Logf("select stale sudog: no false deadlock across %d schedules", res.Runs)
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
