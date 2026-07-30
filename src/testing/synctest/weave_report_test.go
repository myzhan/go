// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

// Unit tests for weave's report rendering and reporting policy. These are the
// functions that decide what a user is told, and they had no coverage at all — which
// is how two silent defects survived: a replay command that omitted -weave for
// failures found through atomic transitions, and t.Skip inside a model being reported
// as a failing interleaving.
package synctest

import (
	"strings"
	"testing"

	"internal/weave"
)

// A seed only reproduces under the build that produced it. Transitions that exist
// solely in a -weave build must therefore put -weave in the printed command; the ones
// that exist in both must not, because rerunning WITH instrumentation would explore a
// different set of scheduling points and the seed would not match.
func TestReplayCommandIncludesWeaveExactlyWhenNeeded(t *testing.T) {
	weaveOnly := []string{
		"read", "write", // compiler instrumentation
		"atomic load", "atomic store", "atomic rmw", // sync/atomic hook
		"rlock", "runlock", "once", // sync package hooks
		"cond wait", "cond signal", "cond broadcast",
		"wg wait", "wg add",
	}
	bothModes := []string{
		"lock", "unlock", // internal/sync.Mutex: always compiled
		"chan send", "chan recv", "chan close", "select", // runtime
		"chan send (nb)", "chan recv (nb)", "clock advance", "run",
	}
	for _, op := range weaveOnly {
		if !traceNeedsWeave([]weave.Step{{Op: op}}) {
			t.Errorf("a trace containing %q needs -weave to reproduce, but the command omits it", op)
		}
	}
	for _, op := range bothModes {
		if traceNeedsWeave([]weave.Step{{Op: op}}) {
			t.Errorf("%q exists without -weave, so adding the flag would change the scheduling "+
				"points and invalidate the seed", op)
		}
	}
	// The flag is decided over the whole trace, not the first step.
	mixed := []weave.Step{{Op: "lock"}, {Op: "chan send"}, {Op: "atomic load"}}
	if !traceNeedsWeave(mixed) {
		t.Errorf("a single weave-only transition anywhere in the trace must select -weave")
	}
}

// The command has to survive a shell. Seeds contain '|' for select choices, and test
// names can contain regexp metacharacters.
func TestReplayCommandIsShellSafe(t *testing.T) {
	cmd := replayCommand("0.1|2.3", "TestFoo/sub+case", nil)
	if !strings.Contains(cmd, `'0.1|2.3'`) {
		t.Errorf("seed with a pipe must be quoted as one shell word: %s", cmd)
	}
	if !strings.Contains(cmd, `\+`) {
		t.Errorf("regexp metacharacters in the test name must be escaped: %s", cmd)
	}
	if !strings.Contains(cmd, "^TestFoo$/^sub\\+case$") {
		t.Errorf("-run pattern must anchor each name segment: %s", cmd)
	}
	if strings.Contains(cmd, "-weave") {
		t.Errorf("a trace with no weave-only transition must not ask for -weave: %s", cmd)
	}
}

// The trace is the main thing a reader looks at, so its shape is worth pinning:
// objects get stable labels, the synthetic clock is not a goroutine, and values are
// shown where they were recorded.
func TestFormatTraceShape(t *testing.T) {
	lab := newAddrLabeler()
	out := formatTrace([]weave.Step{
		{Wid: 1, Op: "lock", Addr: 0x1000},
		{Wid: 2, Op: "chan send", Addr: 0x2000},
		{Wid: 1, Op: "unlock", Addr: 0x1000},
		{Wid: weave.ClockWid, Op: "clock advance", Val: 1e9, HasVal: true},
		{Wid: 2, Op: "read", File: "/a/b/cache_test.go", Line: 31, Val: 7, HasVal: true},
	}, lab)
	for _, want := range []string{
		"g1 lock  mutex#1",     // same address twice keeps the same label
		"g2 chan send  chan#1", // labels are numbered per kind, not globally
		"g1 unlock  mutex#1",
		"clock advance +1s",             // no gN: the clock is not a goroutine
		"g2 read = 7  cache_test.go:31", // value and position, basename only
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "g63") {
		t.Errorf("the synthetic clock must not be printed as a goroutine:\n%s", out)
	}
}

// A deadlock report is only useful if it says what each goroutine is stuck on, and
// only blocking operations qualify: a goroutine whose last step was an unlock is not
// waiting for anything.
func TestDeadlockReportListsWaits(t *testing.T) {
	lab := newAddrLabeler()
	out := formatOutcome(weave.Result{
		Deadlock: true,
		Trace: []weave.Step{
			{Wid: 0, Op: "run"},
			{Wid: 1, Op: "lock", Addr: 0x1000},
			{Wid: 2, Op: "chan recv", Addr: 0x2000},
			{Wid: 3, Op: "unlock", Addr: 0x1000},
		},
	}, lab)
	if !strings.Contains(out, "g1 blocked on lock mutex#1") {
		t.Errorf("blocked goroutine not listed with its object:\n%s", out)
	}
	if !strings.Contains(out, "g2 blocked on chan recv chan#1") {
		t.Errorf("blocked receiver not listed:\n%s", out)
	}
	if strings.Contains(out, "g3") {
		t.Errorf("a goroutine whose last step was an unlock is not blocked:\n%s", out)
	}
	if strings.Contains(out, "g0") {
		t.Errorf("the model root is not a blocked participant:\n%s", out)
	}
}

// A test failure carries its own message (t.Fatal's text), which must be shown as
// written rather than dressed up as a Go panic.
func TestFailureMessageRendering(t *testing.T) {
	if got := formatOutcome(weave.Result{Failed: true, Value: testFailure{"cache_test.go:31: x = 1, want 2"}}, newAddrLabeler()); got != "cache_test.go:31: x = 1, want 2" {
		t.Errorf("a test's own failure text must be shown verbatim, got %q", got)
	}
	if got := formatOutcome(weave.Result{Failed: true, Value: "boom"}, newAddrLabeler()); got != "panic: boom" {
		t.Errorf("a real panic value should be labelled as one, got %q", got)
	}
}

type testFailure struct{ msg string }

func (f testFailure) WeaveTestFailure() string { return f.msg }

// The cross-schedule note must prefer the evidence that names source positions, and
// must stay empty when there is nothing to say — it is appended to reports that are
// otherwise clean, so a spurious note would be noise on every run.
func TestNotReproducibleNote(t *testing.T) {
	if got := notReproducibleNote(weave.Result{}); got != "" {
		t.Errorf("no evidence should produce no note, got %q", got)
	}
	valueLevel := weave.Result{
		CarriedStateReason: "read 1 at cache_test.go:31",
		DivergedReason:     "diverged at step 4",
	}
	if got := notReproducibleNote(valueLevel); !strings.Contains(got, "cache_test.go:31") {
		t.Errorf("the note should prefer the evidence naming source positions, got %q", got)
	}
	warmupOnly := weave.Result{WarmupDivergedReason: "diverged at step 9"}
	if got := notReproducibleNote(warmupOnly); !strings.Contains(got, "step 9") {
		t.Errorf("warm-up evidence should still be citable, got %q", got)
	}
}

// A model that skips itself must be recognized as a skip. t.Skip leaves via
// runtime.Goexit exactly like t.Fatal, so without this the engine's "failed" result
// would be reported as a failing interleaving (it was, until ADR D21).
func TestWeaveSkippedRecognizesSkip(t *testing.T) {
	if !weaveSkipped(weave.Result{Failed: true, Value: testSkip{}}) {
		t.Errorf("a skip sentinel must be recognized as a skip, not a failure")
	}
	if weaveSkipped(weave.Result{Failed: true, Value: "boom"}) {
		t.Errorf("an ordinary panic must not be mistaken for a skip")
	}
	if weaveSkipped(weave.Result{Deadlock: true}) {
		t.Errorf("a deadlock must not be mistaken for a skip")
	}
}

type testSkip struct{}

func (testSkip) WeaveTestSkip() bool { return true }

// The wall-clock backstop exists so a state-space explosion reports INCOMPLETE rather
// than running to the schedule budget; an unset or unparseable value must not disable
// it by accident, while an explicit zero must.
func TestResolveTimeout(t *testing.T) {
	if got := resolveTimeout(""); got != defaultTimeout {
		t.Errorf("unset WEAVE_TIMEOUT should use the default backstop, got %v", got)
	}
	if got := resolveTimeout("not-a-duration"); got != defaultTimeout {
		t.Errorf("an unparseable value must fall back to the default, not disable the limit, got %v", got)
	}
	if got := resolveTimeout("90s"); got.Seconds() != 90 {
		t.Errorf("resolveTimeout(90s) = %v", got)
	}
	if got := resolveTimeout("0"); got != 0 {
		t.Errorf("an explicit zero must disable the limit, got %v", got)
	}
}
