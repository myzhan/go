// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"context"
	"strings"
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

// The timeout side of a "timeout vs event" race must be reachable. The worker here
// is always immediately runnable, so under synctest's rule — advance the clock only
// once the WHOLE bubble is durably blocked — the timer could never fire and "the
// timeout won" was not in the state space at all. Advancing the fake clock is now a
// schedule choice of its own (D22), so the interleaving where the timeout wins and
// strands the worker forever on an unbuffered send (a textbook goroutine leak) must
// be found and reported as a deadlock.
func TestClockAdvanceIsAScheduleChoice(t *testing.T) {
	res := Explore(func() {
		result := make(chan int) // unbuffered: nobody left to receive once we give up
		go func() { result <- 42 }()
		select {
		case <-result:
		case <-time.After(time.Second):
			// gave up; the worker's send can never complete
		}
	})
	if !res.Deadlock {
		t.Fatalf("expected the timeout branch to strand the worker; explored %d schedule(s): %+v", res.Runs, res)
	}
	if !res.ClockAdvanced {
		t.Fatalf("deadlock found without ever advancing the clock — not the interleaving under test: %+v", res.Trace)
	}
	sawAdvance := false
	for _, s := range res.Trace {
		if s.Op != "clock advance" {
			continue
		}
		sawAdvance = true
		if s.Wid != ClockWid {
			t.Errorf("clock advance attributed to wid %d, want the reserved %d", s.Wid, ClockWid)
		}
		if s.Val != uint64(time.Second) {
			t.Errorf("clock advanced by %v, want %v", time.Duration(s.Val), time.Second)
		}
	}
	if !sawAdvance {
		t.Fatalf("failing trace has no clock-advance transition: %+v", res.Trace)
	}
	for _, g := range res.Goroutines {
		if g.Wid == ClockWid {
			t.Errorf("the synthetic clock leaked into the goroutine legend: %+v", g)
		}
	}
	t.Logf("timeout-vs-event found after %d schedule(s); seed %q", res.Runs, res.Seed)
}

// Choosing the clock over a runnable goroutine costs a preemption. That is what
// keeps synctest's idle-only behaviour available: at 0 preemptions the clock still
// advances when nothing else can run (so time.Sleep-style models keep working) but
// can never preempt a runnable goroutine, making WEAVE_MAX_PREEMPTIONS=0 an exact
// opt-out of the D22 relaxation.
func TestClockAdvanceCostsAPreemption(t *testing.T) {
	model := func() {
		result := make(chan int)
		go func() { result <- 42 }()
		select {
		case <-result:
		case <-time.After(time.Second):
		}
	}
	b0 := ExploreBounded(model, DefaultMaxSchedules, 0, 0)
	if b0.Failed || b0.Deadlock {
		t.Fatalf("with 0 preemptions the clock must not preempt a runnable goroutine; got %+v", b0)
	}
	if b0.ClockAdvanced {
		t.Fatalf("with 0 preemptions no schedule should have advanced the clock; got %+v", b0)
	}
	if b0.Truncated {
		t.Fatalf("a preemption bound should not truncate; got %+v", b0)
	}
	b1 := ExploreBounded(model, DefaultMaxSchedules, 1, 0)
	if !b1.Deadlock {
		t.Fatalf("with one preemption allowed the timeout branch should be reached; got %+v", b1)
	}
	t.Logf("clock bounded by preemption count: c=0 clean (%d schedules), c=1 found the leak (%d)", b0.Runs, b1.Runs)
}

// A seed containing a clock advance must replay to the same interleaving: the clock
// is just another wid in the choice vector, and every schedule starts from a fresh
// bubble whose clock is reset, so no extra state needs recording.
func TestClockAdvanceSeedReplays(t *testing.T) {
	model := func() {
		result := make(chan int)
		go func() { result <- 42 }()
		select {
		case <-result:
		case <-time.After(time.Second):
		}
	}
	found := Explore(model)
	if !found.Deadlock || found.Seed == "" {
		t.Fatalf("expected a deadlock with a seed; got %+v", found)
	}
	for i := range 3 {
		rep := Replay(found.Seed, model)
		if !rep.Deadlock {
			t.Fatalf("replay %d: seed %q did not reproduce the deadlock", i, found.Seed)
		}
		if !rep.ClockAdvanced {
			t.Fatalf("replay %d: seed %q replayed without the clock advance", i, found.Seed)
		}
		if !sameSchedule(rep.Trace, found.Trace) {
			t.Fatalf("replay %d: interleaving differs\norig: %v\nrep:  %v", i, found.Trace, rep.Trace)
		}
	}
	t.Logf("clock-advance seed %q replays deterministically", found.Seed)
}

// leakyOnce and leakyCount outlive one schedule on purpose: they are the state a
// model must NOT depend on, and the tests below assert weave says so.
var (
	leakyOnce  sync.Once
	leakyCount int
)

// Exploration works by replaying a prefix of its earlier choices, which is only
// meaningful if the model behaves identically given the same choices. A
// package-level sync.Once breaks that: the first schedule runs the initializer, the
// rest skip it, so from the second schedule onwards weave is exploring a different
// program than the one it recorded. That used to be entirely silent — weave would
// report "explored N schedules, ok" about a model it never actually explored. It must
// now be noticed, with the step named, so the report can carry the caveat.
func TestNonReproducibleModelDetected(t *testing.T) {
	leakyOnce = sync.Once{}
	res := Explore(func() {
		ch := make(chan int, 1)
		go func() {
			leakyOnce.Do(func() { ch <- 1 }) // only the first schedule sends
			ch <- 2
		}()
		<-ch
		Wait()
	})
	// A sync.Once initializes exactly once, so the difference is between the first
	// run and the second; runs 2 and 3 then agree. That is recorded as warm-up
	// evidence rather than a hard error, because the standard library's own lazy
	// initialization looks identical (see WarmupDiverged) — the point of this test is
	// that weave notices at all, and can say where.
	if !res.WarmupDiverged {
		t.Fatalf("a package-level sync.Once makes the first schedule differ from the rest, "+
			"but exploration noticed nothing: %+v after %d schedule(s)", res, res.Runs)
	}
	if res.Failed || res.Deadlock {
		t.Errorf("one-shot state must not be reported as a found bug: %+v", res)
	}
	if !strings.Contains(res.WarmupDivergedReason, "diverged at step") {
		t.Errorf("reason %q does not name the diverging step", res.WarmupDivergedReason)
	}
	t.Logf("one-shot outer state noticed: %s", res.WarmupDivergedReason)
}

// State that keeps changing on every run — as opposed to initializing once — cannot
// be explained away as warm-up: runs 2 and 3 differ too, so the search is exploring a
// moving target. weave must notice and be able to say where, so the report can carry
// the caveat (it is evidence rather than a verdict: see Result.Diverged).
func TestAccumulatingOuterStateReported(t *testing.T) {
	leakyCount = 0
	res := Explore(func() {
		leakyCount++
		n := leakyCount // grows with every schedule, so no two runs agree
		ch := make(chan int, 8)
		go func() {
			for i := 0; i < n; i++ {
				ch <- i
			}
			close(ch)
		}()
		for range ch {
		}
		Wait()
	})
	if !res.Diverged {
		t.Fatalf("state that changes on every run must be noticed; got %+v after %d schedule(s)",
			res, res.Runs)
	}
	if !strings.Contains(res.DivergedReason, "diverged at step") {
		t.Errorf("reason %q does not name the diverging step", res.DivergedReason)
	}
	t.Logf("accumulating outer state reported: %s", res.DivergedReason)
}

// The check must not fire on the common and harmless shape: a model that WRITES
// state outliving the schedule (collecting results, bumping a counter) without its
// own behaviour depending on it. Flagging this would make weave unusable for
// coarse-grained tests, which legitimately touch plenty of outer state.
func TestWriteOnlyOuterStateIsFine(t *testing.T) {
	leakyCount = 0
	collected := []int{} // declared outside the model on purpose
	res := Explore(func() {
		ch := make(chan int, 2)
		go func() { ch <- 1 }()
		go func() { ch <- 2 }()
		a, b := <-ch, <-ch
		leakyCount++                       // accumulates across schedules
		collected = append(collected, a+b) // grows across schedules
		Wait()
	})
	if res.Diverged {
		t.Fatalf("write-only outer state must not be reported as non-reproducible: %s", res.DivergedReason)
	}
	if res.Failed || res.Deadlock {
		t.Fatalf("model is correct; got %+v", res)
	}
	if res.Runs < 2 {
		t.Fatalf("model explored only %d schedule(s), so the cross-run check never ran", res.Runs)
	}
	t.Logf("write-only outer state stayed silent across %d schedule(s) (counter reached %d)", res.Runs, leakyCount)
}

// A "failure" that only happens because an earlier schedule left state behind is
// not a finding: rerunning the very same choices does not reproduce it. weave must
// notice that before handing the reader a counterexample they cannot act on.
//
// The model below panics exactly once — on the schedule where the counter reaches 2 —
// so the confirming rerun sees 3 and no panic. A real concurrency bug reproduces from
// the same choices every time, which the next test asserts.
func TestUnreproducibleFailureIsNotReported(t *testing.T) {
	leakyCount = 0
	res := Explore(func() {
		leakyCount++
		if leakyCount == 2 {
			panic("only on the second schedule")
		}
		ch := make(chan int, 1)
		go func() { ch <- 1 }()
		<-ch
		Wait()
	})
	if !res.NotConfirmed {
		t.Fatalf("a failure that does not rerun must not be reported as a finding; got %+v", res)
	}
	if res.Failed || res.Deadlock {
		t.Errorf("an unconfirmed failure must not also be presented as one: %+v", res)
	}
	if res.Trace != nil {
		t.Errorf("an unconfirmed failure must not carry a trace: %+v", res.Trace)
	}
	t.Logf("unreproducible failure withheld: %s", res.NotConfirmedReason)
}

// The counterpart: a real schedule-dependent bug reproduces when the same choices are
// replayed, so confirmation must not get in the way of reporting it.
func TestReproducibleFailureStillReported(t *testing.T) {
	res := Explore(func() {
		ch := make(chan int, 2)
		done := make(chan bool, 2)
		go func() { ch <- 1; done <- true }()
		go func() { ch <- 2; done <- true }()
		<-done
		<-done
		if first := <-ch; first != 1 {
			panic("order-dependent receive")
		}
	})
	if !res.Failed {
		t.Fatalf("expected the order-dependent panic to be found and reported; got %+v", res)
	}
	if res.NotConfirmed {
		t.Fatalf("a genuine schedule-dependent bug must reproduce on rerun: %s", res.NotConfirmedReason)
	}
	if len(res.Trace) == 0 || res.Seed == "" {
		t.Errorf("a confirmed failure must carry its trace and seed; got %+v", res)
	}
	t.Logf("confirmed failure reported after %d schedule(s), including the confirming rerun", res.Runs)
}

// select's poll order must be deterministic inside a controlled bubble. The runtime
// normally shuffles it (cheaprandn) so that a select over several ready cases is fair;
// weave skips the shuffle, because the explorer identifies a case by its index in the
// ready set, and a shuffled order would make the same choice mean a different case
// from one run to the next. Everything downstream depends on it: seeds would stop
// reproducing and the enumeration would revisit cases while missing others.
//
// Breaking this produces FLAKINESS, not a failure, which is why it needs a test that
// repeats: a shuffled order would pick a different case in roughly half of the runs
// below.
func TestSelectPollOrderIsDeterministic(t *testing.T) {
	const runs = 20
	seen := map[int]int{}
	for range runs {
		Run(func() {
			a := make(chan int, 1)
			b := make(chan int, 1)
			a <- 1
			b <- 2
			var got int
			select { // both cases ready, so the poll order decides which one fires
			case got = <-a:
			case got = <-b:
			}
			selectChoice = got
		})
		seen[selectChoice]++
	}
	// Which case the default policy lands on is not the point (it depends on how the
	// compiler lays the cases out); that it lands on the SAME one every time is.
	if len(seen) != 1 {
		t.Fatalf("the default schedule chose different ready cases across %d runs (%v): "+
			"the poll order is not deterministic, so seeds will not reproduce", runs, seen)
	}
	t.Logf("the default schedule chose the same ready case in all %d runs (%v)", runs, seen)
}

// selectChoice carries the chosen value out of the model above. Written by the model,
// read only after Run returns.
var selectChoice int

// A pending timer that never fired is only worth mentioning when the clock was never
// advanced anywhere — advancing it is a scheduling choice now (D22), so the note is
// gated on ClockAdvanced. These are the two flags the driver's hint is built from, and
// getting the gate backwards would either spam every timer model with advice it does
// not need or silently swallow the one case where the advice matters.
func TestUnfiredTimerAndClockAdvancedFlags(t *testing.T) {
	model := func() {
		done := make(chan int, 1) // buffered: the timeout path leaks nothing
		go func() { done <- 1 }()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	// At zero preemptions the clock cannot preempt the runnable sender, so the timer
	// is still pending when everyone exits: exactly the case worth a hint.
	bounded := ExploreBounded(model, DefaultMaxSchedules, 0, 0)
	if !bounded.UnfiredTimer {
		t.Errorf("a timer left pending at exit should be flagged; got %+v", bounded)
	}
	if bounded.ClockAdvanced {
		t.Errorf("at zero preemptions the clock must never advance; got %+v", bounded)
	}
	// Unbounded, some schedule does advance it, so there is nothing to advise about.
	full := Explore(model)
	if !full.ClockAdvanced {
		t.Errorf("some schedule should have advanced the clock; got %+v", full)
	}
	t.Logf("hint inputs behave: bounded{unfired=%v advanced=%v} unbounded{advanced=%v}",
		bounded.UnfiredTimer, bounded.ClockAdvanced, full.ClockAdvanced)
}

// A seed is a vector of forced choices, so a run that does not follow it is not the
// interleaving the seed describes — the seed came from a different build of the test,
// or the model is not reproducible. weaveChoose falls back to the lowest runnable
// participant when a planned wid cannot run, and that fallback used to be silent,
// which turned a stale seed into the most misleading answer available: "replayed, no
// failure". Unlike divergence during exploration, this one is unambiguous and is
// reported.
func TestReplayRejectsSeedItCannotFollow(t *testing.T) {
	model := func() {
		ch := make(chan int, 1)
		go func() { ch <- 1 }()
		<-ch
		Wait()
	}
	// wid 7 never exists in this model (it has two participants), so the plan cannot
	// be followed and the fallback kicks in.
	res := Replay("0.7.0.0", model)
	if !res.Diverged {
		t.Fatalf("a seed naming a participant that does not exist must be rejected; got %+v", res)
	}
	if !strings.Contains(res.DivergedReason, "diverged at step") {
		t.Errorf("reason %q does not say where the replay left the seed", res.DivergedReason)
	}
	// A seed the model does follow must replay silently.
	good := Explore(func() {
		ch := make(chan int, 1)
		go func() { ch <- 1 }()
		<-ch
		Wait()
		panic("boom")
	})
	if !good.Failed || good.Seed == "" {
		t.Fatalf("setup: expected a seed to replay; got %+v", good)
	}
	rep := Replay(good.Seed, func() {
		ch := make(chan int, 1)
		go func() { ch <- 1 }()
		<-ch
		Wait()
		panic("boom")
	})
	if rep.Diverged {
		t.Errorf("a seed produced by this model must replay without complaint: %s", rep.DivergedReason)
	}
	if !rep.Failed {
		t.Errorf("replaying the failing seed should reproduce the panic; got %+v", rep)
	}
	t.Logf("stale seed rejected: %s", res.DivergedReason)
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

// AB/BA deadlock built from capacity-1 channels used as locks (recv = acquire,
// send = release). This exercises the channelHB token-recycling taint (D16): a
// participant both sends and receives on each lock channel, so the FIFO send→recv
// pairing is not a stable happens-before and must not be used to prune the
// reversal that reaches the deadlock. Regression: before the taint, DPOR explored
// a single schedule and missed this, while the equivalent sync.Mutex version
// (TestMutexDPORFindsDeadlock) was found.
func TestChannelLockDPORFindsDeadlock(t *testing.T) {
	res := Explore(func() {
		lockA := make(chan int, 1)
		lockB := make(chan int, 1)
		lockA <- 1 // token present = unlocked
		lockB <- 1
		go func() {
			<-lockA
			<-lockB
			lockB <- 1
			lockA <- 1
		}()
		go func() {
			<-lockB
			<-lockA
			lockA <- 1
			lockB <- 1
		}()
	})
	if !res.Deadlock {
		t.Fatalf("expected DPOR to find AB/BA channel-lock deadlock; explored %d schedules", res.Runs)
	}
	t.Logf("DPOR found channel-lock deadlock after %d schedule(s); seed %q", res.Runs, res.Seed)
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

// Map iteration order is determinized inside a controlled bubble, so a model whose
// behavior depends on it behaves the same every run.
//
// Asserted by observing the actual first key rather than by whether a panic fires.
// The earlier version compared a.Failed to b.Failed for a model that panicked when
// the first key was a hard-coded 3 — but determinization pins the first key to 12
// here, so that panic never fired and the comparison was false != false regardless
// of whether determinization worked at all. Collect the first key across every
// schedule instead and require exactly one distinct value: if the order were still
// random, the schedules would disagree.
func TestMapIterationDeterministic(t *testing.T) {
	seen := map[int]bool{}
	res := Explore(func() {
		m := map[int]int{}
		for i := 0; i < 16; i++ {
			m[i] = i
		}
		first := -1
		for k := range m {
			first = k
			break
		}
		seen[first] = true // write-only outer state; records what each schedule saw
		Wait()
	})
	if res.Failed || res.Deadlock {
		t.Fatalf("model is correct; got %+v", res)
	}
	if res.Runs < 2 {
		t.Fatalf("only %d schedule ran, so a single map order cannot be distinguished from a "+
			"determinized one", res.Runs)
	}
	if len(seen) != 1 {
		keys := make([]int, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		t.Fatalf("map iteration order is not determinized: %d schedules saw different first "+
			"keys %v; a model depending on map order would be nondeterministic", res.Runs, keys)
	}
	t.Logf("map iteration determinized: every one of %d schedule(s) saw the same first key", res.Runs)
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
