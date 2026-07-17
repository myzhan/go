// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

package synctest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "unsafe" // for go:linkname

	"internal/weave"
)

// testingWeaveTest runs one exploration schedule: it executes f inline in the
// calling goroutine (the weave bubble root) with a fresh child *testing.T, and
// re-surfaces any failure (t.Error/Fatal or panic in f) as a panic so weave
// records the schedule as a failing interleaving. Running f inline — rather than
// via testingSynctestTest's child goroutine — is what makes synctest.Wait()/
// weave.Wait() wait only for f's own goroutines instead of deadlocking on a
// phantom root waiter.
//
//go:linkname testingWeaveTest testing/synctest.testingWeaveTest
func testingWeaveTest(t *testing.T, f func(*testing.T))

// defaultTimeout is the wall-clock backstop applied to one Test under -weave
// when WEAVE_TIMEOUT is not set, so a state-space explosion reports INCOMPLETE
// instead of running up to the schedule budget. It is a soft limit checked
// between schedules; interrupting a single hung schedule still needs
// `go test -timeout`.
const defaultTimeout = 60 * time.Second

// weaveExplore runs f under weave's systematic interleaving exploration instead
// of a single synctest pass. It is only compiled with -weave (which defines the
// "weave" build tag); ordinary builds use the no-op in weave_off.go. Results are
// reported on the real (out-of-bubble) t after exploration finishes.
//
// Environment knobs mirror the engine: WEAVE_REPLAY=<seed> replays one reported
// interleaving; WEAVE_MAX_SCHEDULES=<n> bounds the search; WEAVE_MAX_PREEMPTIONS=<c>
// restricts to schedules with at most c preemptions; WEAVE_TIMEOUT=<dur> overrides
// the wall-clock backstop (0 disables it).
func weaveExplore(t *testing.T, f func(*testing.T)) bool {
	t.Helper()

	body := func() { testingWeaveTest(t, f) }

	if seed := os.Getenv("WEAVE_REPLAY"); seed != "" {
		res := weave.Replay(seed, body)
		switch {
		case !replayInconclusive(res):
			t.Logf("weave: replayed seed %s, no failure", seed)
		case res.Truncated:
			t.Errorf("weave: replay INCOMPLETE (seed %s): %s", seed, res.TruncatedReason)
		default:
			t.Errorf("weave: replayed interleaving (seed %s):\n%s%s%s",
				seed, formatGoroutines(res.Goroutines), formatTrace(res.Trace), outcomeMsg(res))
		}
		return true
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
	timeout := resolveTimeout(os.Getenv("WEAVE_TIMEOUT"))

	res := weave.ExploreBounded(body, budget, maxPreempt, timeout)
	switch {
	case res.Failed, res.Deadlock:
		t.Errorf("weave: found failing interleaving after %d schedule(s):\n%s%s%s\n"+
			"reproduce with: %s",
			res.Runs, formatGoroutines(res.Goroutines), formatTrace(res.Trace), outcomeMsg(res),
			replayCommand(res.Seed, t.Name(), res.Trace))
	case res.Truncated:
		t.Errorf("weave: exploration INCOMPLETE after %d schedule(s): %s; "+
			"no failure found in the explored subset. Reduce the model, or raise the "+
			"limits with WEAVE_MAX_SCHEDULES / WEAVE_TIMEOUT (WEAVE_TIMEOUT=0 disables "+
			"the time limit).", res.Runs, res.TruncatedReason)
	case maxPreempt >= 0:
		t.Logf("weave: ok, explored %d schedule(s) within %d preemption(s)", res.Runs, maxPreempt)
	default:
		t.Logf("weave: ok, explored %d schedule(s)", res.Runs)
	}
	return true
}

// resolveTimeout returns the per-test wall-clock budget from the WEAVE_TIMEOUT
// value: unset uses defaultTimeout; a valid duration is used as-is (0 or negative
// disables the limit); an unparseable value falls back to the default.
func resolveTimeout(env string) time.Duration {
	if env == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(env)
	if err != nil {
		return defaultTimeout
	}
	return d
}

// replayInconclusive reports whether a replayed result must fail the test rather
// than be reported as a clean pass: a panic or deadlock is a reproduced failure,
// and a truncated replay did not run to completion (capacity overflow), so
// treating it as "no failure" would be a false green light.
func replayInconclusive(res weave.Result) bool {
	return res.Failed || res.Deadlock || res.Truncated
}

// replayCommand formats the shell command that reproduces a discovered failure.
// The seed is single-quoted because a select seed contains '|', which the shell
// would otherwise treat as a pipe. A failure found under -weave (where memory
// accesses are extra scheduling points) is only reproducible under -weave, so the
// flag is included exactly when the failing trace contains a memory transition.
func replayCommand(seed, name string, trace []weave.Step) string {
	flag := ""
	if traceNeedsWeave(trace) {
		flag = " -weave"
	}
	return fmt.Sprintf("WEAVE_REPLAY=%s go test%s -run %s", shellSingleQuote(seed), flag, shellSingleQuote(runPattern(name)))
}

// traceNeedsWeave reports whether the failing interleaving contains a memory
// read/write transition. Those exist only under -weave (memory instrumentation),
// and the schedule points differ between the two modes, so such a failure
// reproduces only under -weave.
func traceNeedsWeave(trace []weave.Step) bool {
	for _, s := range trace {
		if s.Op == "read" || s.Op == "write" {
			return true
		}
	}
	return false
}

// runPattern builds a -run value that matches exactly this test (and subtest),
// escaping each slash-separated segment so regexp metacharacters in the name are
// matched literally.
func runPattern(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = "^" + quoteMeta(p) + "$"
	}
	return strings.Join(parts, "/")
}

// quoteMeta escapes regexp metacharacters in s (matching regexp.QuoteMeta), so a
// test name is matched literally by -run. Inlined rather than importing regexp,
// to keep the dependency set minimal (see go/build/deps_test.go).
func quoteMeta(s string) string {
	const special = `\.+*?()|[]{}^$`
	var b strings.Builder
	b.Grow(2 * len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; strings.IndexByte(special, c) >= 0 {
			b.WriteByte('\\')
			b.WriteByte(c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
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
// site of the go statement that spawned it. g0 is the model root.
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

// funcName trims the package path from a fully-qualified function name.
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
		// Drop a bare "run" step immediately followed by a real operation from the
		// same goroutine: the operation line already shows it was scheduled.
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
			val = fmt.Sprintf(" = %d", s.Val)
		}
		n++
		fmt.Fprintf(&b, "  %2d: g%d %s%s%s\n", n, s.Wid, s.Op, val, loc)
	}
	return b.String()
}
