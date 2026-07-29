// This file contains weave demos for asynchronous-IO / cancellation / timeout
// style concurrency, written as ordinary testing/synctest tests.
//
// Modeling time and IO: these demos use in-memory seams that weave explores as
// scheduling points — channels, select, sync primitives, net.Pipe, and context
// cancellation. That is the same idiom synctest/loom/Coyote encourage: it keeps
// the interleaving explicit and deterministic. Time-based code also works (both
// plain synctest and, under -weave, weave drive synctest's fake clock; see
// .claude/weave/design.md ADR D11), but channel/context seams keep these demos
// free of any timer dependence.
package weavedemo

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
)

// TestAsyncWorkerRequestResponse models asynchronous IO with an in-memory
// request/response worker: a background goroutine serves requests over channels
// while the caller awaits responses. weave explores every interleaving of the
// hand-offs and confirms each response is correct. PASS (correct code).
func TestAsyncWorkerRequestResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req := make(chan int)
		resp := make(chan int)
		go func() {
			resp <- (<-req) * 2
			resp <- (<-req) * 2
		}()
		req <- 21
		a := <-resp
		req <- 10
		b := <-resp
		if a != 42 || b != 20 {
			t.Fatal("wrong async response")
		}
	})
}

// TestNetPipeExchange uses net.Pipe — an in-memory, synchronous net.Conn — as a
// fake for real network IO. One goroutine writes, the caller reads; the pipe's
// Read/Write rendezvous is a real synchronization point, so weave explores the
// interleavings without any real sockets or timers. PASS.
func TestNetPipeExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c1, c2 := net.Pipe()
		go func() {
			c1.Write([]byte("hi"))
			c1.Close()
		}()
		buf := make([]byte, 2)
		n, _ := c2.Read(buf)
		c2.Close()
		if n != 2 || string(buf[:n]) != "hi" {
			t.Fatal("bad pipe read")
		}
	})
}

// TestContextCancelPropagation checks that a cancellation signal reaches a
// worker across every interleaving. context.WithCancel is built on a channel +
// mutex (no timer), so weave explores the "cancel before / after the worker
// starts waiting" orderings and the worker always observes ctx.Done(). PASS.
func TestContextCancelPropagation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() {
			<-ctx.Done()
			done <- 1
		}()
		cancel()
		if <-done != 1 {
			t.Fatal("worker did not observe cancellation")
		}
	})
}

// TestTimeoutGoroutineLeak is a classic real-world async bug: a result is
// delivered on an UNBUFFERED channel, but the consumer also has a timeout path.
// When the timeout branch is taken, the worker's `result <- v` has no receiver
// and blocks forever — a leaked goroutine. weave enumerates the select and
// finds the interleaving where the timeout wins, then reports the leak as a
// deadlock with a reproducing seed. EXPECTED TO FAIL under -weave.
//
// The timeout is pre-elapsed (a buffered channel already holding a token) so it
// is ready at the select point, modeling "the deadline already passed".
func TestTimeoutGoroutineLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result := make(chan int) // unbuffered: the bug
		timeout := make(chan struct{}, 1)
		timeout <- struct{}{}        // deadline already elapsed
		go func() { result <- 42 }() // worker tries to deliver
		select {
		case <-result:
		case <-timeout:
			// consumer gives up; the worker's send now leaks.
		}
	})
}

// TestTimeoutLeakFixed is the fix for the bug above: a BUFFERED result channel
// (capacity 1) lets the worker's send complete even if the consumer already
// took the timeout branch, so no goroutine leaks in any interleaving. weave
// explores the schedules and finds no deadlock. PASS.
func TestTimeoutLeakFixed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result := make(chan int, 1) // buffered: the fix
		timeout := make(chan struct{}, 1)
		timeout <- struct{}{}
		go func() { result <- 42 }()
		select {
		case <-result:
		case <-timeout:
		}
	})
}
