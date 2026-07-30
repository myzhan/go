package supported

import (
	"testing"
	"testing/synctest"
)

// TestChannelLockDeadlock is an AB/BA deadlock built from capacity-1 channels
// used as locks (recv = acquire the token, send = release). g1 acquires A then
// B; g2 acquires B then A. In the interleaving where g1 holds A and g2 holds B,
// each blocks forever acquiring the other — the same hazard as the sync.Mutex
// AB/BA deadlock, expressed with channels (a normal way to build locks/semaphores
// in Go). weave finds it and reports the deadlock with a reproducing seed.
// EXPECTED TO FAIL under -weave; PASSES in a single serial synctest schedule.
//
// This case previously escaped weave: channels used as locks recycle a token
// (the same goroutine receives to acquire and sends to release), so the FIFO
// send→recv pairing that weave's DPOR used for happens-before was not stable
// across reorderings and spuriously ordered the two competing receivers, pruning
// this reversal. weave now taints such token-recycling channels (a participant
// that both sends and receives on a channel) so their FIFO order is not treated
// as happens-before, and the deadlock is explored. See .claude/weave/design.md.
func TestChannelLockDeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lockA := make(chan int, 1)
		lockB := make(chan int, 1)
		lockA <- 1 // a present token means "unlocked"
		lockB <- 1
		go func() {
			<-lockA // acquire A
			<-lockB // then B
			lockB <- 1
			lockA <- 1
		}()
		go func() {
			<-lockB // acquire B
			<-lockA // then A (opposite order: the AB/BA hazard)
			lockA <- 1
			lockB <- 1
		}()
		synctest.Wait()
	})
}
