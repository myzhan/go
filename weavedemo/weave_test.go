// Package weavedemo shows the final weave test form: ordinary Go concurrency
// code — real `go func()`, real sync.Mutex, real channels, plain int — written
// as STANDARD testing/synctest tests, with no weave-specific API.
//
// Run the ordinary way for a single deterministic synctest pass:
//
//	../bin/go test -v
//
// Add -weave to turn the SAME synctest tests into systematic interleaving
// exploration. The -weave flag defines the "weave" build tag and enables memory
// instrumentation, so ordinary reads/writes become scheduling points (needed for
// data-race tests like TestLostUpdate; channel/mutex tests explore without it):
//
//	../bin/go test -weave -v
//
// TestLostUpdate, TestCheckThenActOverdraw and TestDeadlock are EXPECTED TO FAIL
// under -weave: that failure is weave reporting the bug, printing the exact
// interleaving and a seed to reproduce it with WEAVE_REPLAY. Without -weave a
// single synctest schedule does not hit the buggy interleaving, so they pass.
// The other tests are correct code and pass in both modes.
package weavedemo

import (
	"sync"
	"testing"
	"testing/synctest"
)

// Lost update: two goroutines run x = x + 1 with no synchronization. Under
// -weave the compiler turns the read and write of x into scheduling points, so
// weave finds the interleaving that reads 0 in both goroutines and leaves
// x == 1. EXPECTED TO FAIL under -weave (weave found the bug).
func TestLostUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := 0
		go func() { x = x + 1 }()
		go func() { x = x + 1 }()
		synctest.Wait() // wait for both goroutines to finish before asserting
		if x != 2 {
			t.Fatal("lost update")
		}
	})
}

// The same increment guarded by a sync.Mutex is always correct: weave explores
// every interleaving and finds no failure.
func TestMutexProtected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		x := 0
		done := make(chan bool, 2)
		inc := func() {
			mu.Lock()
			x = x + 1
			mu.Unlock()
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if x != 2 {
			t.Fatal("lost update")
		}
	})
}

// A real unbuffered channel rendezvous always delivers the value.
func TestChannelRendezvous(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan int)
		go func() { ch <- 42 }()
		if got := <-ch; got != 42 {
			t.Fatal("bad value")
		}
	})
}

// TestCheckThenActOverdraw is a check-then-act (TOCTOU) atomicity violation, a
// classic real-world concurrency bug distinct from a plain lost update. Two
// withdrawals of 60 run against a balance of 100 with the check and the update
// NOT performed atomically. Serialized, only one succeeds (100 -> 40; the second
// sees 40 < 60 and skips). But an interleaving where both read balance == 100
// before either subtracts overdraws the account to -20. Under -weave the
// reads/writes of balance are scheduling points, so weave finds it.
// EXPECTED TO FAIL under -weave (weave reporting the overdraw, with a seed).
func TestCheckThenActOverdraw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		balance := 100
		withdraw := func(amount int) {
			if balance >= amount { // check
				balance -= amount // act (not atomic with the check)
			}
		}
		go func() { withdraw(60) }()
		go func() { withdraw(60) }()
		synctest.Wait()
		if balance < 0 {
			t.Fatal("account overdrawn: balance went negative")
		}
	})
}

// TestCheckThenActGuarded is the fix for the bug above: holding the mutex across
// BOTH the check and the update makes the operation atomic. weave explores every
// interleaving and confirms the balance never goes negative — a passing test
// that shows weave verifying a fix, not just finding a bug.
func TestCheckThenActGuarded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		balance := 100
		withdraw := func(amount int) {
			mu.Lock()
			if balance >= amount {
				balance -= amount
			}
			mu.Unlock()
		}
		go func() { withdraw(60) }()
		go func() { withdraw(60) }()
		synctest.Wait()
		if balance < 0 {
			t.Fatal("account overdrawn: balance went negative")
		}
	})
}

// Classic AB/BA lock-ordering deadlock in real sync.Mutexes. EXPECTED TO FAIL
// under -weave: weave finds the interleaving where each goroutine holds one lock
// and blocks waiting for the other. (Mutex operations are always scheduling
// points, so no memory instrumentation is required for this one.)
func TestDeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
}
