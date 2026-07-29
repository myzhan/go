package supported

import (
	"testing"
	"testing/synctest"
)

// TestCheckThenActOverdraw is a check-then-act (TOCTOU) atomicity violation, a
// classic real-world concurrency bug distinct from a plain lost update. Two
// withdrawals of 60 run against a balance of 100 with the check and the update
// NOT performed atomically. Serialized, only one succeeds (100 -> 40; the second
// sees 40 < 60 and skips). But an interleaving where both read balance == 100
// before either subtracts overdraws the account to -20.
//
// Under -weave the reads/writes of balance are scheduling points, so weave finds
// it. EXPECTED TO FAIL under -weave (weave reporting the overdraw, with a seed).
// The fix — holding one mutex across BOTH the check and the update — is in
// correct_sync_basics_test.go, which weave verifies as correct.
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
