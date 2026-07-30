package supported

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
)

// This file holds CORRECT asynchronous-IO / cancellation / timeout code that weave
// explores and certifies: all EXPECTED TO PASS in both modes.
//
// Modeling time and IO: these demos use in-memory seams that weave explores as
// scheduling points — channels, select, sync primitives, net.Pipe, and context
// cancellation. That is the same idiom synctest/loom/Coyote encourage: it keeps the
// interleaving explicit and deterministic. Time-based code also works (both plain
// synctest and, under -weave, weave drive synctest's fake clock; see
// .claude/weave/design.md ADR D11), but channel/context seams keep these demos free
// of any timer dependence.

// TestAsyncWorkerRequestResponse models asynchronous IO with an in-memory
// request/response worker: a background goroutine serves requests over channels
// while the caller awaits responses. weave explores every interleaving of the
// hand-offs and confirms each response is correct.
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
// interleavings without any real sockets or timers.
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

// TestContextCancelPropagation checks that a cancellation signal reaches a worker
// across every interleaving. context.WithCancel is built on a channel + mutex (no
// timer), so weave explores the "cancel before / after the worker starts waiting"
// orderings and the worker always observes ctx.Done(). Contrast
// context_cancel_missed_test.go, where the worker polls ctx.Err() instead of
// selecting on ctx.Done() and therefore leaks.
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

// TestTimeoutLeakFixed is the fix for timeout_leaks_worker_test.go: a BUFFERED
// result channel (capacity 1) lets the worker's send complete even if the consumer
// already took the timeout branch, so no goroutine leaks in any interleaving.
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
