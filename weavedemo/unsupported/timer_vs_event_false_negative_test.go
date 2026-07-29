package unsupported

import (
	"testing"
	"testing/synctest"
	"time"
)

// TestTimerVsEventFalseNegative shows a subtlety of the fake clock, not a bug in
// the code under test. It races a real time.After(timeout) against a concurrent
// event, but the event goroutine stays runnable, so the fake clock never advances
// to fire the timer — the "timeout wins" interleaving is never explored (a false
// negative). weave PASSES but prints a note: a pending timer never fired. The fix
// on the *test* side is to align the event to the timer boundary (Sleep it to the
// same virtual instant); see .claude/weave/README.md.
func TestTimerVsEventFalseNegative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan int, 1)
		go func() { done <- 1 }() // event is always immediately runnable
		select {
		case <-done:
		case <-time.After(time.Second): // never fires: clock can't advance
		}
	})
}
