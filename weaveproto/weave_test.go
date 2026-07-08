// Package weavedemo shows the final weave test form: ordinary Go concurrency
// code — real `go func()`, real sync.Mutex, real channels, plain int — with no
// special primitives. weave systematically explores the goroutine interleavings.
//
// Run with the built toolchain, enabling memory instrumentation so that ordinary
// reads/writes become scheduling points:
//
//	../bin/go test -gcflags=-weave -v
//
// TestLostUpdate and TestDeadlock are EXPECTED TO FAIL: that failure is weave
// reporting the bug, printing the exact interleaving and a seed to reproduce it
// with WEAVE_REPLAY. The other tests are correct code and pass.
package weavedemo

import (
	"sync"
	"testing"
	"testing/weave"
)

// Lost update: two goroutines run x = x + 1 with no synchronization. Under
// -gcflags=-weave the compiler turns the read and write of x into scheduling
// points, so weave finds the interleaving that reads 0 in both goroutines and
// leaves x == 1. EXPECTED TO FAIL (weave found the bug).
func TestLostUpdate(t *testing.T) {
	weave.Test(t, func() {
		x := 0
		go func() { x = x + 1 }()
		go func() { x = x + 1 }()
		weave.Wait() // join both goroutines before asserting
		if x != 2 {
			panic("lost update")
		}
	})
}

// The same increment guarded by a sync.Mutex is always correct: weave explores
// every interleaving and finds no failure.
func TestMutexProtected(t *testing.T) {
	weave.Test(t, func() {
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
			panic("lost update")
		}
	})
}

// A real unbuffered channel rendezvous always delivers the value.
func TestChannelRendezvous(t *testing.T) {
	weave.Test(t, func() {
		ch := make(chan int)
		go func() { ch <- 42 }()
		if got := <-ch; got != 42 {
			panic("bad value")
		}
	})
}

// Classic AB/BA lock-ordering deadlock in real sync.Mutexes. EXPECTED TO FAIL:
// weave finds the interleaving where each goroutine holds one lock and blocks
// waiting for the other. (No -weave needed: mutex operations are always
// recorded.)
func TestDeadlock(t *testing.T) {
	weave.Test(t, func() {
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
