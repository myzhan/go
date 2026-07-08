// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave_test

import (
	"testing"
	"testing/weave"
)

// A correct counter guarded by a channel handoff: every schedule yields 2, so
// Test passes.
func TestCorrect(t *testing.T) {
	weave.Test(t, func() {
		x := 0
		done := make(chan bool, 2)
		inc := func() {
			x++
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
