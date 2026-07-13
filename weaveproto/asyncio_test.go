// This file contains weave demos for asynchronous-IO / cancellation / timeout
// style concurrency. It also documents an important boundary.
//
// IMPORTANT — time.Sleep and real timers do NOT work inside weave.Test:
// A weave controlled bubble is driven entirely by weave's run-token scheduler,
// which (by design, see .claude/weave/l2-impl-notes.md) skips synctest's
// fake-clock advance loop. So bubble time never advances: a time.Sleep (or
// time.After / time.NewTimer / time.Tick / context.WithTimeout) parks its
// participant on a timer that never fires, every participant ends up blocked,
// and weave reports a *false* "deadlock: all goroutines blocked".
//
// The idiom is the same one synctest/loom/Coyote require: model time and IO
// with in-memory seams that weave already explores as scheduling points —
// channels, select, sync primitives, net.Pipe, and context cancellation
// (context.WithCancel is channel/mutex based and works; context.WithTimeout is
// timer based and does not). The demos below use those seams.
package weavedemo

import (
	"context"
	"net"
	"testing"
	"testing/weave"
)

// TestAsyncWorkerRequestResponse models asynchronous IO with an in-memory
// request/response worker: a background goroutine serves requests over channels
// while the caller awaits responses. weave explores every interleaving of the
// hand-offs and confirms each response is correct. PASS (correct code).
func TestAsyncWorkerRequestResponse(t *testing.T) {
	weave.Test(t, func() {
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
			panic("wrong async response")
		}
	})
}

// TestNetPipeExchange uses net.Pipe — an in-memory, synchronous net.Conn — as a
// fake for real network IO. One goroutine writes, the caller reads; the pipe's
// Read/Write rendezvous is a real synchronization point, so weave explores the
// interleavings without any real sockets or timers. PASS.
func TestNetPipeExchange(t *testing.T) {
	weave.Test(t, func() {
		c1, c2 := net.Pipe()
		go func() {
			c1.Write([]byte("hi"))
			c1.Close()
		}()
		buf := make([]byte, 2)
		n, _ := c2.Read(buf)
		c2.Close()
		if n != 2 || string(buf[:n]) != "hi" {
			panic("bad pipe read")
		}
	})
}

// TestContextCancelPropagation checks that a cancellation signal reaches a
// worker across every interleaving. context.WithCancel is built on a channel +
// mutex (no timer), so weave explores the "cancel before / after the worker
// starts waiting" orderings and the worker always observes ctx.Done(). PASS.
func TestContextCancelPropagation(t *testing.T) {
	weave.Test(t, func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() {
			<-ctx.Done()
			done <- 1
		}()
		cancel()
		if <-done != 1 {
			panic("worker did not observe cancellation")
		}
	})
}

// TestTimeoutGoroutineLeak is a classic real-world async bug: a result is
// delivered on an UNBUFFERED channel, but the consumer also has a timeout path.
// When the timeout branch is taken, the worker's `result <- v` has no receiver
// and blocks forever — a leaked goroutine. weave enumerates the select and
// finds the interleaving where the timeout wins, then reports the leak as a
// deadlock with a reproducing seed. EXPECTED TO FAIL (weave found the bug).
//
// The timeout is pre-elapsed (a buffered channel already holding a token) so it
// is ready at the select point, modeling "the deadline already passed".
func TestTimeoutGoroutineLeak(t *testing.T) {
	weave.Test(t, func() {
		result := make(chan int)      // unbuffered: the bug
		timeout := make(chan struct{}, 1)
		timeout <- struct{}{}         // deadline already elapsed
		go func() { result <- 42 }()  // worker tries to deliver
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
	weave.Test(t, func() {
		result := make(chan int, 1)   // buffered: the fix
		timeout := make(chan struct{}, 1)
		timeout <- struct{}{}
		go func() { result <- 42 }()
		select {
		case <-result:
		case <-timeout:
		}
	})
}
