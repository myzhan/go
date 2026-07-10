// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

// These tests require the -weave build (memory instrumentation):
//
//	go test -weave internal/weave
//
// so that ordinary memory accesses in the model become scheduling points and no
// explicit weave.Yield is needed. The -weave flag defines the "weave" build tag.
package weave

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

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

// --- DPOR soundness differential suite -------------------------------------
//
// For each model, the SET of terminal states DPOR reaches must equal the set an
// exhaustive search reaches: DPOR explores one representative per partial-order
// equivalence class, and all schedules in a class share a terminal state, so a
// sound DPOR reaches every distinct terminal state. Models are inline Go, so
// only shared-variable accesses become scheduling points (keeping exhaustive
// tractable). Requires -gcflags=-weave.

// statesOf runs model under explore once per schedule, collecting every distinct
// terminal state it reports.
func statesOf(explore func(func()) Result, model func() string) map[string]bool {
	set := map[string]bool{}
	explore(func() {
		set[model()] = true
	})
	return set
}

func sameStateSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestDPORSoundnessSuite(t *testing.T) {
	models := []struct {
		name string
		f    func() string
		want []string // if set, assert DPOR reaches exactly these; skip exhaustive
	}{
		{"lost-update-2", func() string {
			x := 0
			go func() { x = x + 1 }()
			go func() { x = x + 1 }()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"lost-update-3", func() string {
			x := 0
			go func() { x = x + 1 }()
			go func() { x = x + 1 }()
			go func() { x = x + 1 }()
			Wait()
			return fmt.Sprint(x)
		}, []string{"1", "2", "3"}}, // exhaustive is intractable here; DPOR handles it
		{"last-writer-wins", func() string {
			x := 0
			go func() { x = 1 }()
			go func() { x = 2 }()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"read-then-write-2vars", func() string {
			x, y := 0, 0
			go func() { y = x + 1 }()
			go func() { x = y + 1 }()
			Wait()
			return fmt.Sprint(x, y)
		}, nil},
		{"mutex-protected", func() string {
			var mu sync.Mutex
			x := 0
			inc := func() { mu.Lock(); x = x + 1; mu.Unlock() }
			go inc()
			go inc()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"one-writer-one-reader", func() string {
			x := 0
			seen := -1
			go func() { x = 1 }()
			go func() { seen = x }()
			Wait()
			return fmt.Sprint(x, seen)
		}, nil},
		{"chan-handoff", func() string {
			// The write to x happens-before the read via the channel, so the read
			// always sees 1: a single terminal state, no race across the handoff.
			x := 0
			seen := -1
			ch := make(chan bool)
			go func() { x = 1; ch <- true }()
			<-ch
			seen = x
			Wait()
			return fmt.Sprint(x, seen)
		}, nil},
		{"waitgroup-race", func() string {
			var wg sync.WaitGroup
			x := 0
			wg.Add(2)
			go func() { x = 1; wg.Done() }()
			go func() { x = 2; wg.Done() }()
			wg.Wait()
			return fmt.Sprint(x)
		}, nil},
		{"three-mutex-inc", func() string {
			var mu sync.Mutex
			x := 0
			inc := func() { mu.Lock(); x = x + 1; mu.Unlock() }
			go inc()
			go inc()
			go inc()
			Wait()
			return fmt.Sprint(x)
		}, nil},
		{"rwmutex", func() string {
			var mu sync.RWMutex
			x := 0
			seen := -1
			go func() { mu.Lock(); x = 1; mu.Unlock() }()
			go func() { mu.RLock(); seen = x; mu.RUnlock() }()
			Wait()
			return fmt.Sprint(x, seen)
		}, nil},
	}

	for _, m := range models {
		t.Run(m.name, func(t *testing.T) {
			dpor := statesOf(Explore, m.f)
			got := sortedKeys(dpor)
			if m.want != nil {
				// Exhaustive is intractable; check DPOR against the known set.
				if !equalStrings(got, m.want) {
					t.Fatalf("DPOR states %v, want %v", got, m.want)
				}
				t.Logf("DPOR states=%v (== expected; exhaustive intractable)", got)
				return
			}
			exh := statesOf(exploreExhaustive, m.f)
			if !sameStateSet(dpor, exh) {
				t.Fatalf("DPOR states %v != exhaustive states %v", got, sortedKeys(exh))
			}
			t.Logf("DPOR states=%v == exhaustive (sound)", got)
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
