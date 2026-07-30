// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

// These tests require the -weave build (memory instrumentation):
//
//	go test -weave internal/weave
//
// so that ordinary memory accesses in the model become scheduling points and no
// explicit weave.Yield is needed. The -weave flag defines the "weave" build tag.
package weave

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Plain x = x + 1 with no Yield: the compiler-inserted read and write of x are
// scheduling points, so some interleaving reads 0 twice and leaves x == 1.
func TestAutoLostUpdate(t *testing.T) {
	res := Explore(func() {
		x := 0
		done := make(chan bool, 2)
		inc := func() {
			x = x + 1
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if x != 2 {
			panic("lost update")
		}
	})
	if !res.Failed {
		t.Fatalf("no lost update found in %d schedules (need -gcflags=-weave)", res.Runs)
	}
	t.Logf("found lost update after %d schedules (auto-instrumented, no Yield)", res.Runs)
}

type point struct{ a, b int }

// Struct field access must be instrumented too: p.a = p.a + 1 races the same way.
func TestAutoStructField(t *testing.T) {
	res := Explore(func() {
		p := &point{}
		done := make(chan bool, 2)
		inc := func() {
			p.a = p.a + 1
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if p.a != 2 {
			panic("lost struct field update")
		}
	})
	if !res.Failed {
		t.Fatalf("no struct-field lost update in %d schedules (need -gcflags=-weave)", res.Runs)
	}
	t.Logf("struct field: found lost update after %d schedules", res.Runs)
}

// Context bounding: with memory instrumentation the read and write of x are
// separate scheduling points, so the lost update needs a preemption between them
// (a switch away from a still-runnable goroutine mid read-modify-write). With 0
// preemptions each goroutine runs its read+write atomically and x is always 2;
// allowing preemptions surfaces the bug. Requires -weave so x is observable.
func TestPreemptionBound(t *testing.T) {
	model := func() {
		x := 0
		inc := func() { x = x + 1 }
		go inc()
		go inc()
		Wait()
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

	b1 := ExploreBounded(model, DefaultMaxSchedules, 1, 0)
	if !b1.Failed {
		t.Fatalf("with a preemption allowed, expected to find the lost update; got %+v", b1)
	}

	full := Explore(model)
	if !full.Failed {
		t.Fatalf("unbounded exploration should also find the lost update")
	}
	t.Logf("preemption bound: c=0 clean (%d schedules), c=1 found bug (%d), unbounded (%d)",
		b0.Runs, b1.Runs, full.Runs)
}

// The preemption bound must constrain the schedule actually executed, not just
// future backtracks. Here g1 blocks then wakes, and the lowest-wid default would
// preempt the running g2 to run the just-woken g1 between g2's read and write —
// producing a lost update that requires a preemption. With the bound correctly
// enforced on the executed schedule (preemption-free default suffix), c=0 must be
// clean; the bug only appears once a preemption is allowed. Regression for the
// bound only filtering backtracks. Requires -weave so the reads/writes of x are
// scheduling points.
func TestPreemptionBoundConstrainsExecuted(t *testing.T) {
	model := func() {
		x := 0
		ch := make(chan int)
		go func() { // g1 (lower wid): blocks, then read-modify-write
			<-ch
			t := x
			x = t + 1
		}()
		go func() { // g2 (higher wid): read, wake g1, then write
			t := x
			ch <- 1
			x = t + 1
		}()
		Wait()
		if x != 2 {
			panic("lost update")
		}
	}
	b0 := ExploreBounded(model, DefaultMaxSchedules, 0, 0)
	if b0.Failed || b0.Deadlock {
		t.Fatalf("c=0 executed a schedule with a preemption and reported its failure; got %+v", b0)
	}
	if b0.Truncated {
		t.Fatalf("preemption bound should not truncate; got %+v", b0)
	}
	b1 := ExploreBounded(model, DefaultMaxSchedules, 1, 0)
	if !b1.Failed {
		t.Fatalf("with one preemption allowed the lost update should be found; got %+v", b1)
	}
	t.Logf("bound constrains executed schedule: c=0 clean (%d), c=1 found bug (%d)", b0.Runs, b1.Runs)
}

// Soundness + reduction: on an instrumented model, DPOR must reach the same
// outcome as exhaustive exploration while running no more schedules.
func TestDPOREquivalence(t *testing.T) {
	model := func() {
		x := 0
		y := 0
		done := make(chan bool, 2)
		// x is shared/conflicting; y is touched by only one worker (independent).
		go func() { x = x + 1; done <- true }()
		go func() { x = x + 1; y = y + 1; done <- true }()
		<-done
		<-done
		if x != 2 {
			panic("lost update")
		}
	}
	dpor := Explore(model)
	exh := exploreExhaustive(model)
	if dpor.Failed != exh.Failed || dpor.Deadlock != exh.Deadlock {
		t.Fatalf("DPOR/exhaustive disagree: dpor=%+v exhaustive=%+v", dpor, exh)
	}
	if !exh.Failed {
		t.Fatalf("exhaustive did not find the lost update (model/instrumentation issue)")
	}
	if dpor.Runs > exh.Runs {
		t.Fatalf("DPOR explored more than exhaustive: dpor=%d exhaustive=%d", dpor.Runs, exh.Runs)
	}
	t.Logf("equivalent outcome; DPOR %d schedules vs exhaustive %d", dpor.Runs, exh.Runs)
}

// --- DPOR soundness differential suite -------------------------------------
//
// For each model, the SET of terminal states DPOR reaches must equal the set an
// exhaustive search reaches: DPOR explores one representative per partial-order
// equivalence class, and all schedules in a class share a terminal state, so a
// sound DPOR reaches every distinct terminal state. Models are inline Go, so
// only shared-variable accesses become scheduling points (keeping exhaustive
// tractable). Requires -gcflags=-weave.

// statesOf runs model under explore once per schedule, collecting every distinct
// terminal state it reports.
func statesOf(explore func(func()) Result, model func() string) map[string]bool {
	set := map[string]bool{}
	explore(func() {
		set[model()] = true
	})
	return set
}

func sameStateSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestDPORSoundnessSuite(t *testing.T) {
	models := []struct {
		name string
		f    func() string
		want []string // if set, assert DPOR reaches exactly these; skip exhaustive
	}{
		{"lost-update-2", func() string {
			x := 0
			go func() { x = x + 1 }()
			go func() { x = x + 1 }()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"lost-update-3", func() string {
			x := 0
			go func() { x = x + 1 }()
			go func() { x = x + 1 }()
			go func() { x = x + 1 }()
			Wait()
			return fmt.Sprint(x)
		}, []string{"1", "2", "3"}}, // exhaustive is intractable here; DPOR handles it
		{"last-writer-wins", func() string {
			x := 0
			go func() { x = 1 }()
			go func() { x = 2 }()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"read-then-write-2vars", func() string {
			x, y := 0, 0
			go func() { y = x + 1 }()
			go func() { x = y + 1 }()
			Wait()
			return fmt.Sprint(x, y)
		}, nil},
		{"mutex-protected", func() string {
			var mu sync.Mutex
			x := 0
			inc := func() { mu.Lock(); x = x + 1; mu.Unlock() }
			go inc()
			go inc()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"one-writer-one-reader", func() string {
			x := 0
			seen := -1
			go func() { x = 1 }()
			go func() { seen = x }()
			Wait()
			return fmt.Sprint(x, seen)
		}, nil},
		{"chan-handoff", func() string {
			// The write to x happens-before the read via the channel, so the read
			// always sees 1: a single terminal state, no race across the handoff.
			x := 0
			seen := -1
			ch := make(chan bool)
			go func() { x = 1; ch <- true }()
			<-ch
			seen = x
			Wait()
			return fmt.Sprint(x, seen)
		}, nil},
		{"select-two-ready", func() string {
			// Both cases are ready; enumerating the select must reach both outcomes.
			// (Exhaustive does not enumerate select cases, so check against a known
			// set rather than exploreExhaustive.)
			a := make(chan int, 1)
			b := make(chan int, 1)
			a <- 1
			b <- 2
			var got int
			select {
			case got = <-a:
			case got = <-b:
			}
			Wait()
			return fmt.Sprint(got)
		}, []string{"1", "2"}},
		{"chan-producer-consumer", func() string {
			// Two producers race to send; the buffered channel is FIFO so the two
			// receive values reflect the send order. x,y are each written before a
			// send, so the channel orders them ahead of nothing shared here.
			ch := make(chan int, 2)
			x, y := 0, 0
			go func() { x = 1; ch <- 1 }()
			go func() { y = 2; ch <- 2 }()
			a := <-ch
			b := <-ch
			Wait()
			return fmt.Sprint(x, y, a, b)
		}, nil},
		{"waitgroup-race", func() string {
			var wg sync.WaitGroup
			x := 0
			wg.Add(2)
			go func() { x = 1; wg.Done() }()
			go func() { x = 2; wg.Done() }()
			wg.Wait()
			return fmt.Sprint(x)
		}, nil},
		{"three-mutex-inc", func() string {
			var mu sync.Mutex
			x := 0
			inc := func() { mu.Lock(); x = x + 1; mu.Unlock() }
			go inc()
			go inc()
			go inc()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"rwmutex", func() string {
			var mu sync.RWMutex
			x := 0
			seen := -1
			go func() { mu.Lock(); x = 1; mu.Unlock() }()
			go func() { mu.RLock(); seen = x; mu.RUnlock() }()
			Wait()
			return fmt.Sprint(x, seen)
		}, nil},
	}

	for _, m := range models {
		t.Run(m.name, func(t *testing.T) {
			dpor := statesOf(Explore, m.f)
			got := sortedKeys(dpor)
			if m.want != nil {
				// Exhaustive is intractable; check DPOR against the known set.
				if !equalStrings(got, m.want) {
					t.Fatalf("DPOR states %v, want %v", got, m.want)
				}
				t.Logf("DPOR states=%v (== expected; exhaustive intractable)", got)
				return
			}
			exh := statesOf(exploreExhaustive, m.f)
			if !sameStateSet(dpor, exh) {
				t.Fatalf("DPOR states %v != exhaustive states %v", got, sortedKeys(exh))
			}
			t.Logf("DPOR states=%v == exhaustive (sound)", got)
		})
	}
}

// A whole-struct write (a composite store, instrumented as a range access that
// records its full width) races with a read of one field. DPOR must treat them
// as conflicting via byte-range overlap, not bare address equality, so both the
// pre-write (0) and post-write (1) outcomes are explored. Regression for the
// range hook dropping the access width. Requires -weave.
func TestStructWholeWriteVsFieldRead(t *testing.T) {
	type pair struct{ a, b int }
	model := func() string {
		var p pair
		seen := -1
		go func() { p = pair{a: 7, b: 1} }() // whole-struct write (range hook)
		go func() { seen = p.b }()           // single-field read
		Wait()
		return fmt.Sprint(seen)
	}
	dpor := statesOf(Explore, model)
	exh := statesOf(exploreExhaustive, model)
	if !sameStateSet(dpor, exh) {
		t.Fatalf("DPOR states %v != exhaustive %v (whole-struct write vs field read not detected as conflicting)",
			sortedKeys(dpor), sortedKeys(exh))
	}
	if !dpor["0"] || !dpor["1"] {
		t.Fatalf("expected both pre-write (0) and post-write (1) outcomes, got %v", sortedKeys(dpor))
	}
	t.Logf("struct write vs field read: DPOR states=%v == exhaustive (sound)", sortedKeys(dpor))
}

// When a reader parks at its read and another participant writes the location
// before the reader is resumed, the trace must record the value the reader
// actually observes (the written one), not the stale value present when it first
// reached the read. DPOR reverses the racing read/write, so the failing schedule
// is exactly "writer runs, then reader reads 1"; the recorded read value must be
// 1. Regression for sampling the read value before the scheduling point.
// Requires -weave.
func TestReadValueReflectsGrantTime(t *testing.T) {
	model := func() {
		x := 0
		go func() { x = 1 }()
		go func() {
			if x == 1 {
				panic("read observed the write")
			}
		}()
		Wait()
	}
	res := Explore(model)
	if !res.Failed {
		t.Fatalf("expected to find the interleaving where the read observes 1; got %+v", res)
	}
	foundRead1 := false
	for _, s := range res.Trace {
		if s.Op == "read" && s.HasVal && s.Val == 1 {
			foundRead1 = true
		}
	}
	if !foundRead1 {
		t.Fatalf("failing trace records no read with the observed value 1 (stale sampling?); trace=%+v", res.Trace)
	}
	t.Logf("read value reflects grant-time state: observed 1 in the failing interleaving")
}

// Soundness invariant: with a non-blocking recv consuming a buffered value while
// a plain recv and a racy read run concurrently, DPOR must reach the same
// terminal states as an exhaustive search. This exercises the channelHB path
// where a non-blocking recv consumes without popping the FIFO (the taint fix
// suppresses the potential stale-clock edge). NOTE: this is a soundness-invariant
// guard, not a proven load-bearing regression — the fix is conservatively sound
// (it only removes happens-before edges), and this specific model does not by
// itself demonstrate a missed state without it (see the discussion of the
// stale-clock temporal coupling). Requires -weave.
func TestChannelHBNoStaleEdge(t *testing.T) {
	model := func() string {
		ch := make(chan int, 2)
		x := 0
		go func() { x = 1; ch <- 1 }() // g0: writes x, then sends (oldest)
		go func() { ch <- 2 }()        // g1: sends second
		go func() {                    // gA: non-blocking recv consumes the oldest (g0's) value
			select {
			case <-ch:
			default:
			}
		}()
		<-ch   // main: plain recv (may get g1's value, racing g0's write)
		b := x // racy read of x
		Wait()
		return fmt.Sprint(b)
	}
	dpor := statesOf(Explore, model)
	exh := statesOf(exploreExhaustive, model)
	if !sameStateSet(dpor, exh) {
		t.Fatalf("DPOR states %v != exhaustive %v (stale send-clock produced a false happens-before edge)",
			sortedKeys(dpor), sortedKeys(exh))
	}
	t.Logf("channel HB with non-blocking consumer: DPOR states=%v == exhaustive (sound)", sortedKeys(dpor))
}

// Under -weave the instrumentation must cover only the command-line package, not
// its dependencies: instrumenting sync's internals turns RWMutex's private memory
// operations into scheduling points and breaks transition reduction, making DPOR
// miss the TryRLock==false outcome. With the scope limited to the command-line
// package, only the Lock/TryRLock transitions matter and DPOR stays sound. This
// test fails if -weave regresses to instrumenting dependencies. Requires -weave.
func TestRWMutexTryRLockSound(t *testing.T) {
	model := func(rec func(bool)) func() {
		return func() {
			var mu sync.RWMutex
			go func() { mu.Lock(); mu.Unlock() }()
			ok := mu.TryRLock()
			if ok {
				mu.RUnlock()
			}
			Wait()
			rec(ok)
		}
	}
	dpor := map[bool]bool{}
	Explore(model(func(v bool) { dpor[v] = true }))
	exh := map[bool]bool{}
	exploreExhaustive(model(func(v bool) { exh[v] = true }))
	if dpor[true] != exh[true] || dpor[false] != exh[false] {
		t.Fatalf("RWMutex.TryRLock: DPOR states %v != exhaustive %v (dependency instrumentation broke reduction?)", dpor, exh)
	}
	if !dpor[true] || !dpor[false] {
		t.Fatalf("RWMutex.TryRLock: expected both success and failure, DPOR=%v exhaustive=%v", dpor, exh)
	}
	t.Logf("RWMutex.TryRLock sound under -weave: DPOR=%v == exhaustive", dpor)
}

// Sampling a read value must not dereference a bad user address from the
// controller context, where a fault would be a process-fatal. weaveread samples
// in participant context after the scheduling point, so a faulting read is a
// recoverable panic captured by Explore. Regression: a nil dereference under
// -weave is reported as a failure, not a crash. Requires -weave.
func TestReadFaultRecoverable(t *testing.T) {
	model := func() {
		var p *int
		_ = *p // faults; weaveread samples this address in participant context
	}
	res := Explore(model)
	if !res.Failed {
		t.Fatalf("expected the faulting read to be captured as a failure, got %+v", res)
	}
	t.Logf("faulting read captured as a recoverable failure (not a process fatal)")
}

// A read-modify-write built from sync/atomic's typed Load + Store is not atomic as
// a whole, so some interleaving must lose an update. Finding it requires the typed
// atomic operations themselves to be scheduling points (ADR D18): the model touches
// no plain memory between the Load and the Store, so compiler instrumentation alone
// cannot break them apart. Regression for the atomic hook going missing (e.g. if the
// weave build tag stopped reaching sync/atomic, or the hook were inlined away).
func TestAtomicRMWLostUpdate(t *testing.T) {
	res := Explore(func() {
		var x atomic.Int64
		go func() { x.Store(x.Load() + 1) }()
		go func() { x.Store(x.Load() + 1) }()
		Wait()
		if x.Load() != 2 {
			panic("lost atomic update")
		}
	})
	if !res.Failed {
		t.Fatalf("expected the non-atomic Load+Store RMW to lose an update; explored %d schedules (atomic hook missing?)", res.Runs)
	}
	sawAtomic := false
	for _, s := range res.Trace {
		if s.Op == "atomic load" || s.Op == "atomic store" {
			sawAtomic = true
		}
	}
	if !sawAtomic {
		t.Fatalf("failing trace has no atomic transition, so the bug was found some other way: %+v", res.Trace)
	}
	t.Logf("atomic RMW lost update found after %d schedule(s)", res.Runs)
}

// Assigning a multi-field struct is not atomic, so a reader can observe a TORN
// value: one field updated and the other not. Finding it requires -weave to store a
// pointer-free multi-field struct field-by-field, with a scheduling point before
// each field store (ADR D19) — a single whole-struct store would be one indivisible
// transition. Regression for that ssagen change (note it only takes effect after
// reinstalling the compiler: go install cmd/compile).
func TestStructTearingObservable(t *testing.T) {
	type point struct{ x, y int }
	res := Explore(func() {
		var p point // invariant: x == y, the two fields are written together
		go func() { p = point{x: 5, y: 5} }()
		go func() {
			q := p
			if q.x != q.y {
				panic("torn struct read")
			}
		}()
		Wait()
	})
	if !res.Failed {
		t.Fatalf("expected a torn read of the two-field struct; explored %d schedules (field-by-field store missing? rebuild cmd/compile)", res.Runs)
	}
	t.Logf("struct tearing observed after %d schedule(s)", res.Runs)
}

// carriedInt outlives one schedule on purpose (see the two tests below).
var carriedInt int

// The evidence that actually helps a reader is not "you touched outer state" but
// "this schedule read a value an earlier schedule left" — and where both happened.
// Requires -weave, since it is read out of the memory transitions.
func TestCarriedStateNamesBothPositions(t *testing.T) {
	carriedInt = 0
	res := Explore(func() {
		n := carriedInt // reads what the previous schedule left
		carriedInt = n + 1
		ch := make(chan int, 1)
		go func() { ch <- 1 }()
		<-ch
		Wait()
	})
	if !res.CarriedState {
		t.Fatalf("a counter read from a previous schedule must be noticed; got %+v after %d schedule(s)",
			res, res.Runs)
	}
	// Both the read and the earlier write should be named, by file:line.
	for _, want := range []string{"instr_test.go", "read ", "but the first schedule read "} {
		if !strings.Contains(res.CarriedStateReason, want) {
			t.Errorf("evidence %q does not mention %q", res.CarriedStateReason, want)
		}
	}
	t.Logf("carried state: %s", res.CarriedStateReason)
}

// Writing outer state without reading it back is normal and must stay silent: that
// is what keeps weave usable on coarse-grained tests.
func TestWriteOnlyCarriedStateIsSilent(t *testing.T) {
	carriedInt = 0
	collected := []int{} // outside the model, written every schedule
	res := Explore(func() {
		ch := make(chan int, 2)
		go func() { ch <- 1 }()
		go func() { ch <- 2 }()
		a, b := <-ch, <-ch
		carriedInt = a + b                 // write-only: never read back
		collected = append(collected, a+b) // ditto
		Wait()
	})
	if res.CarriedState {
		t.Fatalf("write-only outer state must not be reported: %s", res.CarriedStateReason)
	}
	if res.Failed || res.Deadlock || res.NotConfirmed {
		t.Fatalf("model is correct; got %+v", res)
	}
	if res.Runs < 3 {
		t.Fatalf("only %d schedule(s): the cross-schedule check never had two runs to compare", res.Runs)
	}
	t.Logf("write-only outer state stayed silent across %d schedule(s) (collected %d results)",
		res.Runs, len(collected))
}

// A model that reads a POINTER it has not written must stay silent, even though the
// value differs between runs: it differs because it points at a freshly allocated
// object, not because anything crossed a schedule boundary. Regression for the first
// version of this check, which reported every double-checked-locking style model —
// and, formatting a pointer-sized value, panicked while doing it.
func TestPointerValuesAreNotCarriedState(t *testing.T) {
	type cfg struct{ val int }
	res := Explore(func() {
		var mu sync.Mutex
		var instance *cfg // freshly allocated each schedule; its address is not
		get := func() *cfg {
			if instance == nil { // reads a pointer it has not written yet
				mu.Lock()
				if instance == nil {
					instance = &cfg{val: 42}
				}
				mu.Unlock()
			}
			return instance
		}
		go func() { _ = get() }()
		go func() {
			if c := get(); c != nil && c.val != 42 {
				panic("half-constructed object")
			}
		}()
		Wait()
	})
	if res.CarriedState {
		t.Fatalf("a pointer read is not evidence of carried state: %s", res.CarriedStateReason)
	}
	if res.Failed || res.Deadlock || res.NotConfirmed {
		t.Fatalf("model is correct under sequential consistency; got %+v", res)
	}
	t.Logf("pointer-valued reads stayed silent across %d schedule(s)", res.Runs)
}

// Every transition should carry the source position the user would recognize, not
// the position inside the primitive that implements it. The library hooks are called
// from within sync/sync-atomic, so they reach past themselves and past the method
// that called them (see runtime.weaveSchedPointSkip) — a fixed frame count that this
// test pins down, because a refactor of those helpers would silently shift it and
// every report would start blaming sync/mutex.go instead of the caller.
func TestTransitionsCarryCallerPosition(t *testing.T) {
	var mu sync.Mutex
	var flag atomic.Bool
	ch := make(chan int)
	var lockLine, atomicLine, chanLine int
	res := Explore(func() {
		go func() {
			mu.Lock() // lockLine
			_, _, lockLine, _ = runtime.Caller(0)
			mu.Unlock()
			flag.Store(true) // atomicLine
			_, _, atomicLine, _ = runtime.Caller(0)
			ch <- 1 // chanLine
			_, _, chanLine, _ = runtime.Caller(0)
		}()
		<-ch
		Wait()
		panic("report the trace") // the only way to get the trace out
	})
	if !res.Failed {
		t.Fatalf("expected the model to panic so its trace is reported; got %+v", res)
	}
	// runtime.Caller(0) reports the line it is called on, one below the operation.
	want := map[string]int{"lock": lockLine - 1, "atomic store": atomicLine - 1, "chan send": chanLine - 1}
	got := map[string]int{}
	for _, s := range res.Trace {
		if _, ok := want[s.Op]; ok && got[s.Op] == 0 {
			got[s.Op] = s.Line
		}
	}
	for op, wantLine := range want {
		if got[op] != wantLine {
			t.Errorf("%q transition reported line %d, want %d (the caller's line) — the frame skip in the %s hook is off",
				op, got[op], wantLine, op)
		}
	}
	if !t.Failed() {
		t.Logf("lock/atomic/chan transitions all report the caller's line (%d/%d/%d)",
			got["lock"], got["atomic store"], got["chan send"])
	}
}

// Every primitive weave claims to instrument must actually leave its own transition
// in the trace. The models elsewhere in this file assert OUTCOMES, and an outcome can
// usually be reached some other way — under -weave every memory access is a
// scheduling point too, so a missing lock or Once hook is invisible to them. Deleting
// the whole sync-package hook set (RWMutex, Once, Cond, WaitGroup) used to leave all
// 56 tests here and all 37 demos passing, with only the exploration time quietly
// collapsing. This test is the direct check that each op is recorded.
func TestEveryPrimitiveIsASchedulingPoint(t *testing.T) {
	res := Explore(func() {
		var mu sync.Mutex
		var rw sync.RWMutex
		var once sync.Once
		var wg sync.WaitGroup
		var flag atomic.Bool
		cond := sync.NewCond(&sync.Mutex{})
		ch := make(chan int, 1)
		other := make(chan int, 1)

		mu.Lock()
		mu.Unlock()
		if mu.TryLock() {
			mu.Unlock()
		}
		rw.Lock()
		rw.Unlock()
		rw.RLock()
		rw.RUnlock()
		once.Do(func() {})
		flag.Store(true)
		_ = flag.Load()
		flag.CompareAndSwap(true, false)

		// The rendezvous forces the waiter to reach cond.Wait before the predicate is
		// set: otherwise the default schedule runs the setter first and Wait never
		// happens, so the op would be missing for a reason unrelated to instrumentation.
		ready := false
		reached := make(chan int)
		wg.Add(1)
		go func() {
			cond.L.Lock()
			reached <- 1
			for !ready {
				cond.Wait()
			}
			cond.L.Unlock()
			wg.Done()
		}()
		<-reached     // the waiter holds cond.L here
		cond.L.Lock() // blocks until Wait releases it
		ready = true
		cond.Signal()
		cond.Broadcast()
		cond.L.Unlock()
		wg.Wait()

		ch <- 1
		<-ch
		other <- 2
		select { // one case plus default compiles to a non-blocking receive
		case <-other:
		default:
		}
		ch <- 3
		other <- 4
		select { // two real cases exercise selectgo itself
		case <-ch:
		case <-other:
		}
		close(ch)
		Wait()
		panic("report the trace") // the trace is only surfaced on failure
	})
	if !res.Failed {
		t.Fatalf("expected the model to panic so its trace is reported; got %+v", res)
	}
	seen := map[string]bool{}
	for _, s := range res.Trace {
		seen[s.Op] = true
	}
	// One entry per hook that must exist. rlock/runlock are distinct from lock/unlock
	// on purpose (D20), so a read lock recorded as "lock" is also a failure here.
	for _, op := range []string{
		"lock", "unlock", "rlock", "runlock", "once",
		"cond wait", "cond signal", "cond broadcast", "wg add", "wg wait",
		"atomic load", "atomic store", "atomic rmw",
		"chan send", "chan recv", "chan close", "chan recv (nb)", "select",
	} {
		if !seen[op] {
			t.Errorf("no %q transition in the trace: that primitive is not a scheduling point", op)
		}
	}
	if !t.Failed() {
		t.Logf("all %d instrumented primitives recorded their own transition", len(seen))
	}
}

// The read lock in particular needs its own behavioural check, because an outcome
// test can reach the same state through the memory accesses around it. Here the two
// RLock calls have NOTHING between them — no memory access, no other operation — so
// the only way a writer can slip in between and deadlock the reader (RWMutex is not
// re-entrant and gives writers priority) is if RLock itself is a scheduling point.
func TestReadLockIsASchedulingPoint(t *testing.T) {
	res := Explore(func() {
		var rw sync.RWMutex
		go func() {
			rw.Lock()
			rw.Unlock()
		}()
		rw.RLock()
		rw.RLock() // blocks behind the queued writer, which holds the first RLock
		rw.RUnlock()
		rw.RUnlock()
	})
	if !res.Deadlock {
		t.Fatalf("re-entrant RLock with a queued writer must deadlock in some schedule; "+
			"explored %d schedule(s): %+v — is the RLock hook still there?", res.Runs, res)
	}
	t.Logf("re-entrant read lock deadlock found after %d schedule(s)", res.Runs)
}

// --- sync-primitive hooks (weave-only since ADR D20) -----------------------
//
// RWMutex/Once/Cond/WaitGroup hooks are compiled in under the "weave" build tag,
// so these models are only meaningfully explored here. (Mutex and channel ops are
// recorded unconditionally and are covered in internal_test.go.)

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
// spurious deadlock. (Cond ops are scheduling points only under -weave, see D20;
// joining via weave.Wait keeps the state space small.)
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

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
