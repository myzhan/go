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
		case weaveSkipped(res):
			t.SkipNow()
		case !replayInconclusive(res):
			t.Logf("weave: replayed seed %s, no failure", seed)
		case res.Truncated:
			t.Errorf("weave: replay INCOMPLETE (seed %s): %s", seed, res.TruncatedReason)
		default:
			lab := newAddrLabeler()
			t.Errorf("weave: replayed interleaving (seed %s):\n%s%s%s",
				seed, formatGoroutines(res.Goroutines), formatTrace(res.Trace, lab), formatOutcome(res, lab))
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

	// Iterative deepening on the preemption bound: when the user does not pin
	// WEAVE_MAX_PREEMPTIONS, search 0..defaultPreemptCeiling instead of unbounded
	// (which explodes on realistic models). A pinned value raises the ceiling.
	ceiling := defaultPreemptCeiling
	if maxPreempt >= 0 {
		ceiling = maxPreempt
	}
	res := exploreIterative(body, budget, ceiling, timeout)
	switch {
	case weaveSkipped(res):
		// The model skipped itself; there is nothing to explore. The skip reason was
		// already logged, so just skip the real test.
		t.SkipNow()
	case res.Failed, res.Deadlock:
		lab := newAddrLabeler()
		t.Errorf("weave: found failing interleaving after %d schedule(s):\n%s%s%s\n"+
			"reproduce with: %s",
			res.Runs, formatGoroutines(res.Goroutines), formatTrace(res.Trace, lab), formatOutcome(res, lab),
			replayCommand(res.Seed, t.Name(), res.Trace))
	case res.Truncated:
		hint := ""
		if maxPreempt < 0 {
			hint = " raise WEAVE_MAX_PREEMPTIONS to search deeper, or"
		}
		t.Errorf("weave: exploration INCOMPLETE after %d schedule(s): %s;"+
			" no failure found in the explored subset.%s reduce the model or raise"+
			" WEAVE_MAX_SCHEDULES / WEAVE_TIMEOUT (WEAVE_TIMEOUT=0 disables the time limit).",
			res.Runs, res.TruncatedReason, hint)
	default:
		note := ""
		if maxPreempt < 0 {
			note = " (default ceiling; set WEAVE_MAX_PREEMPTIONS to search deeper)"
		}
		t.Logf("weave: ok, explored %d schedule(s) up to %d preemption(s)%s", res.Runs, ceiling, note)
		if res.UnfiredTimer && !res.ClockAdvanced {
			// Advancing the clock is a scheduling choice now (D22), so a stranded timer
			// only matters when that choice was never taken anywhere — which within a
			// bounded search means the bound forbade it.
			t.Logf("weave: note: a pending timer never fired and the fake clock was never " +
				"advanced in any explored schedule, so the \"timeout wins\" side of a " +
				"timeout-vs-event race is unexplored. Advancing the clock while a goroutine " +
				"is still runnable costs a preemption, so raise WEAVE_MAX_PREEMPTIONS to reach it.")
		}
	}
	return true
}

// defaultPreemptCeiling bounds the automatic search when WEAVE_MAX_PREEMPTIONS is
// unset. Most concurrency bugs surface within a couple of preemptions (the CHESS
// insight), so exploring 0..ceiling keeps an unset run from exploding on
// realistic models while still giving a meaningful "no failure up to K
// preemptions" guarantee. Users raise it via WEAVE_MAX_PREEMPTIONS.
const defaultPreemptCeiling = 2

// exploreIterative runs iterative-deepening context-bounded search: it explores
// schedules with 0,1,...,ceiling preemptions in order, stopping at the first
// failing/deadlocking schedule (so the reported counterexample uses the fewest
// preemptions) or when the schedule/time budget is exhausted. Each level reuses
// ExploreBounded, whose bound is complete within itself; the deepest level
// subsumes the lower ones, so its Runs is the count reported on success.
func exploreIterative(body func(), budget, ceiling int, timeout time.Duration) weave.Result {
	start := time.Now()
	var last weave.Result
	spent := 0
	unfired := false  // any level left a timer pending at exit (aggregated across levels)
	advanced := false // any level advanced the fake clock as a scheduling choice
	for c := 0; c <= ceiling; c++ {
		remBudget := budget
		if budget > 0 {
			remBudget = budget - spent
			if remBudget <= 0 {
				last.Truncated = true
				last.TruncatedReason = fmt.Sprintf("schedule budget reached at %d preemption(s)", c)
				last.UnfiredTimer = unfired
				last.ClockAdvanced = advanced
				return last
			}
		}
		remTime := timeout
		if timeout > 0 {
			remTime = timeout - time.Since(start)
			if remTime <= 0 {
				last.Truncated = true
				last.TruncatedReason = fmt.Sprintf("time budget reached at %d preemption(s)", c)
				last.UnfiredTimer = unfired
				last.ClockAdvanced = advanced
				return last
			}
		}
		res := weave.ExploreBounded(body, remBudget, c, remTime)
		spent += res.Runs
		unfired = unfired || res.UnfiredTimer
		advanced = advanced || res.ClockAdvanced
		if res.Failed || res.Deadlock || res.Truncated {
			res.UnfiredTimer = unfired
			res.ClockAdvanced = advanced
			return res
		}
		last = res // level c fully explored, no failure
	}
	last.UnfiredTimer = unfired
	last.ClockAdvanced = advanced
	return last
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

// weaveSkipped reports whether the model skipped itself (t.Skip/SkipNow inside the
// bubble). Skips travel out as a panic because they run runtime.Goexit, so without
// this check they would be reported as a failing interleaving.
func weaveSkipped(res weave.Result) bool {
	if !res.Failed {
		return false
	}
	s, ok := res.Value.(interface{ WeaveTestSkip() bool })
	return ok && s.WeaveTestSkip()
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

// traceNeedsWeave reports whether the failing interleaving contains a transition
// that only exists in a -weave build, in which case the seed reproduces only under
// -weave (the scheduling points, and hence the choice vector, differ between the
// two modes). Those are memory read/write transitions (compiler instrumentation)
// plus every hook that is compiled in under the "weave" build tag: sync/atomic and
// the sync package's RWMutex-read/Once/Cond/WaitGroup hooks. Channel, select and
// plain Mutex transitions exist in both modes — internal/sync's mutex hook is
// always compiled (see internal/sync/weave.go).
func traceNeedsWeave(trace []weave.Step) bool {
	for _, s := range trace {
		switch s.Op {
		case "read", "write", // compiler instrumentation
			"atomic load", "atomic store", "atomic rmw", // sync/atomic hook
			"rlock", "runlock", "once", // sync package hooks
			"cond wait", "cond signal", "cond broadcast",
			"wg wait", "wg add":
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

// formatOutcome renders the outcome line(s) of a failing schedule. For a panic
// it shows the recovered value; for a deadlock it additionally lists what each
// still-blocked goroutine is waiting on, derived from each participant's last
// trace step (the scheduling point it parked at). lab is shared with formatTrace
// so object labels (mutex#1, chan#2, ...) match between the trace and this list.
func formatOutcome(res weave.Result, lab *addrLabeler) string {
	if res.Failed {
		// A test that failed via t.Error/Fatal carries its own message; show it
		// directly rather than mislabeling it as a Go panic.
		if tf, ok := res.Value.(interface{ WeaveTestFailure() string }); ok {
			return tf.WeaveTestFailure()
		}
		return fmt.Sprintf("panic: %v", res.Value)
	}
	if !res.Deadlock {
		return ""
	}
	bw := blockedWaits(res.Trace)
	if len(bw) == 0 {
		return "deadlock: all goroutines blocked"
	}
	var b strings.Builder
	b.WriteString("deadlock: all goroutines blocked\n")
	for _, w := range bw {
		fmt.Fprintf(&b, "    g%d blocked on %s %s\n", w.wid, w.op, lab.label(w.addr, w.op))
	}
	return b.String()
}

type blockedWait struct {
	wid  int
	op   string
	addr uint64
}

// blockedWaits infers, for a deadlocked schedule, what each participant is stuck
// on: its last recorded trace step is the scheduling point it parked at. Only
// participants whose last step is a blocking operation (and not the model root)
// are reported, in first-appearance order.
func blockedWaits(steps []weave.Step) []blockedWait {
	last := map[int]weave.Step{}
	order := []int{}
	for _, s := range steps {
		if _, seen := last[s.Wid]; !seen {
			order = append(order, s.Wid)
		}
		last[s.Wid] = s
	}
	var out []blockedWait
	for _, wid := range order {
		if wid == 0 {
			continue // model root
		}
		s := last[wid]
		if isBlockingOp(s.Op) {
			out = append(out, blockedWait{wid: wid, op: s.Op, addr: s.Addr})
		}
	}
	return out
}

// isBlockingOp reports whether an operation can leave a goroutine parked waiting
// (so its appearance as a participant's last step means "blocked here"). Ops
// that never block (unlock, close, signal, add, non-blocking select cases, plain
// memory access) are excluded.
func isBlockingOp(op string) bool {
	switch op {
	case "lock", "rlock", "chan send", "chan recv", "cond wait", "wg wait":
		return true
	}
	return false
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

func formatTrace(steps []weave.Step, lab *addrLabeler) string {
	var b strings.Builder
	n := 0
	for i, s := range steps {
		// Drop a bare "run" step immediately followed by a real operation from the
		// same goroutine: the operation line already shows it was scheduled.
		if s.Op == "run" && i+1 < len(steps) && steps[i+1].Wid == s.Wid {
			continue
		}
		if s.Wid == weave.ClockWid {
			// The synthetic clock is not a goroutine, so it gets no gN; show how far
			// time moved instead, which is what makes a timeout-vs-event trace legible.
			n++
			fmt.Fprintf(&b, "  %2d: clock advance +%v\n", n, time.Duration(s.Val))
			continue
		}
		loc := ""
		switch {
		case s.File != "":
			loc = fmt.Sprintf("  %s:%d", filepath.Base(s.File), s.Line)
		case s.Addr != 0:
			loc = "  " + lab.label(s.Addr, s.Op)
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

// addrLabeler maps opaque object addresses to short, stable, human-readable
// labels (mutex#1, chan#2, ...) so a trace/deadlock report identifies "the same
// object" across steps instead of printing raw pointers. The kind is inferred
// from the operation that touched the address; the same address always maps to
// the same label within one report. Shared by formatTrace and the deadlock
// report so both name the same object consistently.
type addrLabeler struct {
	labels map[uint64]string
	counts map[string]int
}

func newAddrLabeler() *addrLabeler {
	return &addrLabeler{labels: map[uint64]string{}, counts: map[string]int{}}
}

func (l *addrLabeler) label(addr uint64, op string) string {
	if s, ok := l.labels[addr]; ok {
		return s
	}
	k := opKind(op)
	l.counts[k]++
	s := fmt.Sprintf("%s#%d", k, l.counts[k])
	l.labels[addr] = s
	return s
}

// opKind classifies an operation name into the object kind it acts on, so the
// generated label reflects what the address is (a mutex, a channel, ...).
func opKind(op string) string {
	switch {
	case op == "lock" || op == "unlock":
		return "mutex"
	case op == "rlock" || op == "runlock":
		return "rwmutex"
	case strings.HasPrefix(op, "chan"):
		return "chan"
	case strings.HasPrefix(op, "cond"):
		return "cond"
	case strings.HasPrefix(op, "wg"):
		return "wg"
	case op == "once":
		return "once"
	case op == "read" || op == "write":
		return "mem"
	default:
		return "obj"
	}
}
