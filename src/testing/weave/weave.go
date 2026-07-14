// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package weave provides support for exhaustively testing concurrent code by
// exploring goroutine interleavings.
//
// [Test] runs a model function under many distinct schedules, serializing the
// goroutines it starts and systematically enumerating the orders in which they
// interleave at synchronization points (channel operations, mutexes, and
// explicit [Yield] calls). If any schedule deadlocks or panics (for example via
// a failed assertion), Test reports it and fails the test.
//
// The model must be self-contained and re-runnable: it is executed many times,
// so all shared state must be created inside the model, not captured from an
// outer scope.
//
//	func TestCounter(t *testing.T) {
//		weave.Test(t, func() {
//			x := 0
//			done := make(chan bool, 2)
//			inc := func() { x++; done <- true }
//			go inc()
//			go inc()
//			<-done
//			<-done
//			if x != 2 {
//				panic("lost update")
//			}
//		})
//	}
//
// Channel and mutex operations are always scheduling points, so tests that
// exercise those (deadlocks, lost signals, lock-ordering bugs) need no special
// flag. To also explore data races on ordinary variables (for example a lost
// update on a plain int), build with the -weave flag, which instruments memory
// accesses:
//
//	go test -weave
//
// Without -weave, a read-modify-write race is only explored if a [Yield] is
// placed between the read and the write.
package weave

import (
	"fmt"
	"internal/weave"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Test explores interleavings of the model f and fails t on the first schedule
// that deadlocks or panics, printing the interleaving and a seed to reproduce
// it. On success it logs how many schedules were explored.
//
// Setting WEAVE_REPLAY=<seed> replays exactly one previously-reported
// interleaving instead of exploring, e.g.
//
//	WEAVE_REPLAY=0.1.1.0 go test -run TestX
//
// Setting WEAVE_MAX_SCHEDULES=<n> bounds how many schedules are explored before
// giving up; if the state space is not exhausted within the budget, Test reports
// the result as incomplete rather than passing.
//
// Setting WEAVE_MAX_PREEMPTIONS=<c> restricts the search to schedules with at
// most c preemptions (context bounding); most concurrency bugs surface with very
// few, so a small c finds them while exploring far fewer schedules.
//
// Setting WEAVE_TIMEOUT=<dur> (e.g. 2s) stops exploration after that much
// wall-clock time, reporting the result as incomplete rather than passing.
func Test(t *testing.T, f func()) {
	t.Helper()

	if seed := os.Getenv("WEAVE_REPLAY"); seed != "" {
		res := weave.Replay(seed, f)
		if res.Failed || res.Deadlock {
			t.Errorf("weave: replayed interleaving (seed %s):\n%s%s%s",
				seed, formatGoroutines(res.Goroutines), formatTrace(res.Trace), outcomeMsg(res))
		} else {
			t.Logf("weave: replayed seed %s, no failure", seed)
		}
		return
	}

	budget := weave.DefaultMaxSchedules
	if v := os.Getenv("WEAVE_MAX_SCHEDULES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			budget = n
		}
	}
	maxPreempt := -1
	if v := os.Getenv("WEAVE_MAX_PREEMPTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxPreempt = n
		}
	}
	var timeout time.Duration
	if v := os.Getenv("WEAVE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	res := weave.ExploreBounded(f, budget, maxPreempt, timeout)
	switch {
	case res.Failed, res.Deadlock:
		t.Errorf("weave: found failing interleaving after %d schedule(s):\n%s%s%s\n"+
			"reproduce with: %s",
			res.Runs, formatGoroutines(res.Goroutines), formatTrace(res.Trace), outcomeMsg(res), replayCommand(res.Seed, t.Name()))
	case res.Truncated:
		// Exploration did not exhaust the state space, so "no failure found" is
		// inconclusive; fail loudly rather than give an unreliable green light.
		t.Errorf("weave: exploration INCOMPLETE after %d schedule(s): %s; "+
			"no failure found in the explored subset. Reduce the model or raise "+
			"the budget with WEAVE_MAX_SCHEDULES.", res.Runs, res.TruncatedReason)
	case maxPreempt >= 0:
		t.Logf("weave: ok, explored %d schedule(s) within %d preemption(s)", res.Runs, maxPreempt)
	default:
		t.Logf("weave: ok, explored %d schedule(s)", res.Runs)
	}
}

// replayCommand formats the shell command that reproduces a discovered failure.
// The seed is single-quoted because a select seed contains '|', which the shell
// would otherwise treat as a pipe. The build mode is preserved: a failure found
// under -weave (where memory accesses are extra scheduling points) is only
// reproducible under -weave, so the flag is included exactly when this binary was
// built with it — running the plain command would explore a different schedule
// space and could spuriously pass.
func replayCommand(seed, name string) string {
	flag := ""
	if builtWithWeave {
		flag = " -weave"
	}
	return fmt.Sprintf("WEAVE_REPLAY=%s go test%s -run %s", shellSingleQuote(seed), flag, shellSingleQuote("^"+name+"$"))
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes, so
// it is a single safe shell word regardless of characters like '|' or '$'.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func outcomeMsg(res weave.Result) string {
	if res.Failed {
		return fmt.Sprintf("panic: %v", res.Value)
	}
	if res.Deadlock {
		return "deadlock: all goroutines blocked"
	}
	return ""
}

// formatGoroutines renders the legend mapping each gN in the trace to the source
// site of the go statement that spawned it, so the numbered trace below is
// readable. g0 is the model root (no spawning go statement).
func formatGoroutines(gs []weave.Goroutine) string {
	if len(gs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  goroutines:\n")
	for _, g := range gs {
		switch {
		case g.Wid == 0:
			fmt.Fprintf(&b, "    g0: model root\n")
		case g.File != "":
			fmt.Fprintf(&b, "    g%d: %s (%s:%d)\n", g.Wid, funcName(g.Func), filepath.Base(g.File), g.Line)
		default:
			fmt.Fprintf(&b, "    g%d: (creation site unknown)\n", g.Wid)
		}
	}
	return b.String()
}

// funcName trims the package path from a fully-qualified function name, keeping
// the last path segment (e.g. "weavedemo.TestX.func1").
func funcName(fn string) string {
	if fn == "" {
		return "?"
	}
	if i := strings.LastIndexByte(fn, '/'); i >= 0 {
		return fn[i+1:]
	}
	return fn
}

func formatTrace(steps []weave.Step) string {
	var b strings.Builder
	n := 0
	for i, s := range steps {
		// Drop a bare "run" scheduling step when it is immediately followed by a
		// real operation from the same goroutine: the "g1 run / g1 read" pair is
		// redundant, the operation line already shows g1 was scheduled.
		if s.Op == "run" && i+1 < len(steps) && steps[i+1].Wid == s.Wid {
			continue
		}
		loc := ""
		switch {
		case s.File != "":
			loc = fmt.Sprintf("  %s:%d", filepath.Base(s.File), s.Line)
		case s.Addr != 0:
			loc = fmt.Sprintf("  @%#x", s.Addr)
		}
		val := ""
		if s.HasVal {
			// The value read (read) or the value written (write).
			val = fmt.Sprintf(" = %d", s.Val)
		}
		n++
		fmt.Fprintf(&b, "  %2d: g%d %s%s%s\n", n, s.Wid, s.Op, val, loc)
	}
	return b.String()
}

// Yield is an explicit scheduling point: it offers to hand execution to another
// goroutine in the model. Use it to expose an interleaving where the model has
// no other synchronization operation.
func Yield() { weave.Yield() }

// Wait blocks until every other goroutine started in the model has finished. Use
// it to join spawned goroutines before asserting a post-condition, instead of a
// channel or sync.WaitGroup.
func Wait() { weave.Wait() }
