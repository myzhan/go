package supported

import (
	"testing"
	"testing/synctest"
	"time"
)

// TestTimerVsEvent races a real time.After timeout against a concurrent event, and
// the timeout path leaks the worker: the result channel is unbuffered, so once the
// consumer gives up nobody will ever receive, and the worker's send blocks forever.
// This is the same textbook bug as timeout_leaks_worker_test.go, but reached through
// a REAL timer rather than a pre-elapsed token — which is what makes it interesting.
//
// It used to be weave's canonical false negative. synctest's fake clock only
// advances once the whole bubble is durably blocked, and the worker goroutine here
// is always immediately runnable, so the timer could never fire: "the timeout won"
// was not merely unexplored, it was not in the state space at all. weave PASSED and
// printed a note suggesting you restructure the test.
//
// Advancing the clock is now a scheduling choice of its own (see
// .claude/weave/design.md D22), so weave explores "the timeout fires first", finds
// the stranded worker, and reports the deadlock with a trace that shows the advance:
//
//	3: clock advance +1s
//
// EXPECTED TO FAIL under -weave. Note the choice costs a preemption, so
// WEAVE_MAX_PREEMPTIONS=0 restores the old idle-only behaviour and this test passes
// again. The fix in real code is a buffered result channel, or having the worker
// select on a cancellation channel.
func TestTimerVsEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result := make(chan int) // unbuffered: the bug
		go func() { result <- 42 }()
		select {
		case <-result:
		case <-time.After(time.Second):
			// gave up; the worker's send can never complete
		}
	})
}
