// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weaveinstr

// These tests require compiler instrumentation: build with
//
//	go test -tags weaveinstr -gcflags=-weave internal/weave
//
// so that ordinary memory accesses in the model become scheduling points and no
// explicit weave.Yield is needed.
package weave

import "testing"

// Plain x = x + 1 with no Yield: the compiler-inserted read and write of x are
// scheduling points, so some interleaving reads 0 twice and leaves x == 1.
func TestAutoLostUpdate(t *testing.T) {
	res := Explore(func() {
		x := 0
		done := make(chan bool, 2)
		inc := func() {
			x = x + 1
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if x != 2 {
			panic("lost update")
		}
	})
	if !res.Failed {
		t.Fatalf("no lost update found in %d schedules (need -gcflags=-weave)", res.Runs)
	}
	t.Logf("found lost update after %d schedules (auto-instrumented, no Yield)", res.Runs)
}

type point struct{ a, b int }

// Struct field access must be instrumented too: p.a = p.a + 1 races the same way.
func TestAutoStructField(t *testing.T) {
	res := Explore(func() {
		p := &point{}
		done := make(chan bool, 2)
		inc := func() {
			p.a = p.a + 1
			done <- true
		}
		go inc()
		go inc()
		<-done
		<-done
		if p.a != 2 {
			panic("lost struct field update")
		}
	})
	if !res.Failed {
		t.Fatalf("no struct-field lost update in %d schedules (need -gcflags=-weave)", res.Runs)
	}
	t.Logf("struct field: found lost update after %d schedules", res.Runs)
}

// Soundness + reduction: on an instrumented model, DPOR must reach the same
// outcome as exhaustive exploration while running no more schedules.
func TestDPOREquivalence(t *testing.T) {
	model := func() {
		x := 0
		y := 0
		done := make(chan bool, 2)
		// x is shared/conflicting; y is touched by only one worker (independent).
		go func() { x = x + 1; done <- true }()
		go func() { x = x + 1; y = y + 1; done <- true }()
		<-done
		<-done
		if x != 2 {
			panic("lost update")
		}
	}
	dpor := Explore(model)
	exh := exploreExhaustive(model)
	if dpor.Failed != exh.Failed || dpor.Deadlock != exh.Deadlock {
		t.Fatalf("DPOR/exhaustive disagree: dpor=%+v exhaustive=%+v", dpor, exh)
	}
	if !exh.Failed {
		t.Fatalf("exhaustive did not find the lost update (model/instrumentation issue)")
	}
	if dpor.Runs > exh.Runs {
		t.Fatalf("DPOR explored more than exhaustive: dpor=%d exhaustive=%d", dpor.Runs, exh.Runs)
	}
	t.Logf("equivalent outcome; DPOR %d schedules vs exhaustive %d", dpor.Runs, exh.Runs)
}
