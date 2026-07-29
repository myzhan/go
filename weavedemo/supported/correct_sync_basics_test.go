package supported

import (
	"sync"
	"testing"
	"testing/synctest"
)

// This file holds the CORRECT counterparts of the bugs demonstrated elsewhere in
// this package: properly synchronized code that weave explores exhaustively and
// finds no failure in. They are EXPECTED TO PASS in both modes — that is the
// point. A bug finder that cannot certify correct code is not useful, so these
// guard against weave reporting false positives on idiomatic Go.

// TestMutexProtected is the fix for lost_update_test.go: the increment guarded by
// a sync.Mutex is correct under every interleaving.
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

// TestChannelRendezvous checks that a real unbuffered channel rendezvous always
// delivers the value, whichever side arrives first.
func TestChannelRendezvous(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan int)
		go func() { ch <- 42 }()
		if got := <-ch; got != 42 {
			t.Fatal("bad value")
		}
	})
}

// TestCheckThenActGuarded is the fix for check_then_act_overdraw_test.go: holding
// the mutex across BOTH the check and the update makes the operation atomic, so
// the balance never goes negative in any interleaving.
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
