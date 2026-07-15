// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave_test

import (
	"sync"
	"testing"
	"testing/weave"
)

// A correct counter: the increment is guarded by a mutex, so every interleaving
// yields 2 and Test passes — including under -weave, where the reads and writes
// of x are scheduling points (an unsynchronized x++ would be a lost update).
func TestCorrect(t *testing.T) {
	weave.Test(t, func() {
		var mu sync.Mutex
		x := 0
		done := make(chan bool, 2)
		inc := func() {
			mu.Lock()
			x++
			mu.Unlock()
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if x != 2 {
			panic("counter wrong")
		}
	})
}
