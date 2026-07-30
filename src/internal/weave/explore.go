// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"runtime"
	"time"
)

// Operation codes, kept in sync with runtime weaveOp.
const (
	opNone uint8 = iota
	opRead
	opWrite
	opLock
	opUnlock
	opChanSend
	opChanRecv
	opChanClose
	opSelect
	opWaitGroupWait
	opGoStart
	opGoExit
	opPreempt
	opWaitGroupAdd
	opCondWait
	opCondSignal
	opCondBroadcast
	opOnce
	opChanSendNB // non-blocking send (select default); scheduling point, no HB
	opChanRecvNB // non-blocking recv (select default); scheduling point, no HB
	opAtomicLoad // sync/atomic load; scheduling point, conflicts by address, no HB
	opAtomicStore
	opAtomicRMW // atomic read-modify-write (Add/Swap/CAS/And/Or)
	opRLock     // RWMutex read lock (shared); conflicts like opLock for now
	opRUnlock   // RWMutex read unlock
	// opClockAdvance advances the fake clock to the next deadline and fires the due
	// timers. It is performed by the controller on behalf of the synthetic clock
	// participant (ClockWid), not by any goroutine. See design.md D22.
	opClockAdvance
)

// ClockWid is the participant id reserved for the synthetic clock. Transitions
// with this wid are clock advances, not goroutine steps: they have no creation
// site and must not appear in the goroutine legend. Kept in sync with
// runtime.weaveClockWid (optab_test.go checks the op codes; this one is asserted
// by TestClockWidReserved).
const ClockWid = 63

// Step is one transition in a recorded interleaving.
type Step struct {
	Wid    int    // participant id
	Op     string // operation kind
	Addr   uint64 // object/variable address, or 0
	File   string // source file of the operation, or "" if unknown
	Line   int    // source line, or 0
	Val    uint64 // scalar value read, or value written (see HasVal)
	HasVal bool   // whether Val holds a meaningful value for this step
}

// Goroutine describes a participant in the failing interleaving: its stable id
// and the source site of the go statement that spawned it.
type Goroutine struct {
	Wid  int    // participant id (the "gN" in the trace)
	File string // source file of the go statement, or "" if unknown/root
	Line int    // source line of the go statement, or 0
	Func string // enclosing function of the go statement, or "" if unknown
}

// Result summarizes an exploration.
type Result struct {
	Runs       int         // schedules explored
	Deadlock   bool        // a schedule deadlocked
	Failed     bool        // a schedule panicked (Value holds the recovered value)
	Value      any         // recovered panic value from the failing schedule
	Seed       string      // reproducible seed for the failing/deadlocking schedule
	Trace      []Step      // the failing/deadlocking interleaving
	Goroutines []Goroutine // participants in the failing interleaving, by wid

	// Diverged is set when replaying the same choices did not reproduce the same
	// participant sequence, i.e. the model does not behave identically across runs.
	// Exploration works by replaying earlier choices, so this undermines everything
	// the search concludes — but it is EVIDENCE, not a verdict, and the driver must
	// not fail a test on it alone. Anything the model does through the *testing.T it
	// was handed appends to the parent test's output, so a model that merely logs is
	// non-reproducible by construction. DivergedReason names the step and what
	// differed, for citing in whatever report is actually made.
	//
	// Replay is the exception: there a mismatch means the seed does not describe this
	// model at all, which is unambiguous and reported as an error.
	Diverged       bool
	DivergedReason string

	// NotConfirmed is set when a reported failure did not happen again on rerunning
	// the very same choices. The counterexample is then not something the reader can
	// act on: either it depends on state an earlier schedule left behind, or weave
	// itself has a bug. Reported instead of the trace, since presenting an
	// unreproducible interleaving as a finding is worse than admitting the doubt.
	NotConfirmed       bool
	NotConfirmedReason string

	// CarriedState records that the model read a location it had not written in that
	// run and got a different value than the first schedule read there — a value that
	// flowed from one schedule into the next. Evidence only, never a failure of its
	// own: CarriedStateReason names both source positions so a report can point at
	// the read and at whatever wrote it.
	CarriedState       bool
	CarriedStateReason string

	// WarmupDiverged records that the model's FIRST run differed from its second,
	// while runs 2 and 3 agreed — the signature of state initialized exactly once.
	// This is not an error on its own: the standard library does it all the time
	// (sync.Pool registers itself under a lock on first use, lazy singletons abound),
	// so failing here would make weave unusable for any model that calls fmt.Sprint
	// or t.Log. Exploration proceeds on the post-warm-up model, which is
	// self-consistent. The evidence is kept so other reports can cite it: when the
	// one-shot state is the MODEL's own (a package-level sync.Once or cache), the
	// schedules that initialize it are exactly the ones going unexplored.
	WarmupDiverged       bool
	WarmupDivergedReason string

	// Truncated is set when exploration stopped before the state space was
	// exhausted (schedule budget reached, or a capacity limit hit). A truncated
	// run that found no failure is inconclusive, not a clean pass; callers must
	// not report it as success. TruncatedReason gives a short explanation.
	Truncated       bool
	TruncatedReason string

	// UnfiredTimer is set if any explored schedule finished with all participants
	// exited while a timer was still pending. ClockAdvanced is set if any explored
	// schedule advanced the fake clock as a scheduling choice (see D22). Together
	// they tell the driver whether a stranded timer is worth reporting: if the clock
	// was never advanced anywhere, the timeout side of a "timeout vs event" race was
	// out of reach (typically because the preemption bound forbade it).
	UnfiredTimer  bool
	ClockAdvanced bool
}

// Run executes f once in a controlled bubble (a single default schedule).
// It panics if the run deadlocks.
func Run(f func()) {
	_, _, outcome, failure, _ := runSchedule(f, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false)
	if failure != nil {
		panic(failure)
	}
	if outcome == 1 {
		panic("weave: deadlock: all goroutines in bubble are blocked")
	}
	if outcome == 2 {
		panic("weave: capacity limit exceeded (too many runnable goroutines or trace overflow)")
	}
}

const traceCap = 1 << 16

type explorer struct {
	traceWid     []int32
	traceOp      []int32
	traceAddr    []int64
	traceSize    []int64 // memory op access width (bytes); 0 for non-memory ops
	traceEnabled []uint64
	tracePC      []uint64
	spawnPC      []uint64 // per-wid creation-site PC, indexed by wid (max 64 participants + slot 0)
	traceVal     []uint64 // scalar value at each memory op
	traceValSet  []int32  // 1 if traceVal[i] is meaningful
	selTrace     []int32  // chosen case index per select event
	selBranch    []int32  // number of ready cases per select event
	selStepIdx   []int32  // scheduling-step index of each select event
	res          Result
	f            func() // the model; panics (main or spawned) are captured by the runtime wrapper

	// The transitions the next run must reproduce, snapshotted when a backtrack is
	// scheduled (see snapshotPrefix/checkPrefix).
	expectWid   []int32
	expectOp    []int32
	expectPC    []uint64
	expectValid bool
	expectFull  bool // the snapshot is a whole trace, so the length must match too
	expectSoft  bool // a mismatch is warm-up evidence, not a hard error
	probe       int  // 0 = warm-up run pending, 1 = baseline run pending, 2 = probe done

	// Cross-schedule state evidence: the first value each location was seen to hold
	// before being written, and where it was last written (see checkCarriedState).
	firstRead map[int64]readObs
	lastWrite map[int64]uint64
}

func newExplorer(f func()) *explorer {
	return &explorer{
		lastWrite:    map[int64]uint64{},
		traceWid:     make([]int32, traceCap),
		traceOp:      make([]int32, traceCap),
		traceAddr:    make([]int64, traceCap),
		traceSize:    make([]int64, traceCap),
		traceEnabled: make([]uint64, traceCap),
		tracePC:      make([]uint64, traceCap),
		spawnPC:      make([]uint64, 65),
		traceVal:     make([]uint64, traceCap),
		traceValSet:  make([]int32, traceCap),
		selTrace:     make([]int32, traceCap),
		selBranch:    make([]int32, traceCap),
		selStepIdx:   make([]int32, traceCap),
		f:            f,
	}
}

// conflict reports whether two transitions by different participants are
// dependent: they touch the same object and their order can change the outcome.
// Unknown ops (opNone, from a freshly spawned or just-woken participant) and
// distinct addresses are independent. Read/read on the same address is
// independent; any write, or any synchronization-object operation, conflicts.
func conflict(opi uint8, ai, si int64, opj uint8, aj, sj int64) bool {
	// A clock advance fires the due timers, and their effect reaches participants
	// only through synchronization: a send on the timer's channel, a goready, or a
	// goroutine spawned by AfterFunc. It writes no user memory, so it COMMUTES with
	// plain reads and writes — which is what lets weavePushClock skip memory
	// scheduling points without losing coverage. Against synchronization it must be
	// conservative, and address-agnostically so: its transition records no address,
	// and a select's readiness can hinge on a timer channel it examines. Checked
	// before the select case below, which would otherwise call the pair independent.
	if opi == opClockAdvance || opj == opClockAdvance {
		other := opi
		if opi == opClockAdvance {
			other = opj
		}
		return other == opClockAdvance || isSyncOp(other)
	}
	// A select's outcome (which case fires, or default) depends on the readiness
	// of every channel it examines, but its transition records only a single
	// address (0), so that dependency cannot be matched by address. Conservatively
	// treat a select as conflicting with any channel operation and with any other
	// select, address-agnostically, so the reordering of a select against a
	// competing send/recv/close is explored. Sound: this over-approximates
	// dependency (never drops a needed reversal). A select does not touch plain
	// memory or locks, so it is independent of those.
	if opi == opSelect || opj == opSelect {
		other := opi
		if opi == opSelect {
			other = opj
		}
		return other == opSelect || isChanOp(other)
	}
	if ai == 0 || aj == 0 {
		return false
	}
	// Memory read/write: conflict when the accessed byte ranges overlap and at
	// least one is a write. Using width (not just the base address) is essential
	// for composite accesses: a whole-object write (range hook) records its full
	// size, so it correctly conflicts with a read of any sub-field, which a bare
	// address-equality test would miss.
	if isMemOp(opi) && isMemOp(opj) {
		if !overlap(ai, si, aj, sj) {
			return false
		}
		if opi == opRead && opj == opRead {
			return false
		}
		return opi == opWrite || opj == opWrite
	}
	// Synchronization objects use identity: the same object address conflicts.
	if ai != aj {
		return false
	}
	// Two atomic loads of the same location are both read-only, so their order
	// does not matter: independent (mirrors read/read on plain memory).
	if opi == opAtomicLoad && opj == opAtomicLoad {
		return false
	}
	if isSyncOp(opi) || isSyncOp(opj) {
		return true
	}
	if opi == opRead && opj == opRead {
		return false
	}
	return opi == opWrite || opj == opWrite
}

func isMemOp(op uint8) bool { return op == opRead || op == opWrite }

// overlap reports whether the byte ranges [a, a+sa) and [b, b+sb) intersect.
// A zero/unknown width is treated as a single byte so an unsized access still
// conflicts with anything touching its address.
func overlap(a, sa, b, sb int64) bool {
	if sa <= 0 {
		sa = 1
	}
	if sb <= 0 {
		sb = 1
	}
	return a < b+sb && b < a+sa
}

// channelHB returns a happens-before predicate for one recorded schedule,
// capturing the ordering that channel operations establish: the k-th send on a
// channel happens-before the k-th receive (Go channels are FIFO), and a close
// happens-before a later receive on the (drained) channel. Program order is
// included via per-participant vector clocks.
//
// Only channel edges are modeled; every other synchronization (mutex, cond,
// waitgroup, ...) is treated as unordered. That is sound for DPOR: a missing
// happens-before edge can only make the search consider more reorderings, never
// skip a needed one. Mutex/RWMutex are deliberately excluded because a single
// acquire/release chain cannot express reader concurrency without risking a
// false ordering (which would drop a real reversal).
func channelHB(wid, op []int32, addr []int64, steps int) func(i, j int) bool {
	// A channel whose FIFO order we cannot trust for happens-before: a
	// non-blocking op (opChanSendNB/RecvNB) may consume a value that our FIFO
	// model does not account for (it does not pop the send queue), and a select
	// consumes from one of several channels we cannot identify (its transition
	// records address 0). Either can leave a stale sender clock that a later plain
	// recv would then pop and be wrongly ordered against — a spurious edge that
	// could prune a needed reversal (unsound). So we suppress channel HB for a
	// tainted channel, and for every channel in a run that contains a select.
	// Program order still applies; fewer edges only widens the search.
	//
	// A channel used as a lock/semaphore is tainted for a further reason: when the
	// SAME participant both sends and receives on it (acquires a token by receiving
	// and releases it by sending), the FIFO send→recv pairing is not stable across
	// reorderings — which send a receive pairs with depends on the schedule. Using
	// that pairing as happens-before spuriously orders two competing receivers and
	// prunes the reversal that reaches an AB/BA deadlock over channel locks. So if
	// any participant both sends and receives on a channel, suppress its HB too.
	tainted := map[int64]bool{}
	hasSelect := false
	chanSenders := map[int64]map[int32]bool{} // channel -> set of participants that sent
	chanRecvers := map[int64]map[int32]bool{} // channel -> set of participants that received
	note := func(m map[int64]map[int32]bool, c int64, p int32) {
		s := m[c]
		if s == nil {
			s = map[int32]bool{}
			m[c] = s
		}
		s[p] = true
	}
	for i := 0; i < steps; i++ {
		switch uint8(op[i]) {
		case opChanSendNB, opChanRecvNB:
			tainted[addr[i]] = true
		case opChanSend:
			note(chanSenders, addr[i], wid[i])
		case opChanRecv:
			note(chanRecvers, addr[i], wid[i])
		case opSelect:
			hasSelect = true
		}
	}
	// Taint channels where a participant both sends and receives (token recycling).
	for c, senders := range chanSenders {
		for p := range senders {
			if chanRecvers[c][p] {
				tainted[c] = true
				break
			}
		}
	}
	P := 0
	for i := 0; i < steps; i++ {
		if int(wid[i])+1 > P {
			P = int(wid[i]) + 1
		}
	}
	clone := func(c []int32) []int32 { d := make([]int32, P); copy(d, c); return d }
	join := func(dst, src []int32) {
		for k := range dst {
			if src[k] > dst[k] {
				dst[k] = src[k]
			}
		}
	}
	cur := make([][]int32, P)
	for p := range cur {
		cur[p] = make([]int32, P)
	}
	sendq := map[int64][][]int32{} // FIFO of sender clocks per channel
	closed := map[int64][]int32{}  // clock published by close, per channel
	vc := make([][]int32, steps)
	ts := make([]int32, steps)
	for i := 0; i < steps; i++ {
		p := int(wid[i])
		skip := hasSelect || tainted[addr[i]]
		if !skip && uint8(op[i]) == opChanRecv {
			if q := sendq[addr[i]]; len(q) > 0 {
				join(cur[p], q[0])
				sendq[addr[i]] = q[1:]
			} else if rc, ok := closed[addr[i]]; ok {
				join(cur[p], rc)
			}
		}
		cur[p][p]++
		vc[i] = clone(cur[p])
		ts[i] = cur[p][p]
		if !skip {
			switch uint8(op[i]) {
			case opChanSend:
				sendq[addr[i]] = append(sendq[addr[i]], clone(cur[p]))
			case opChanClose:
				closed[addr[i]] = clone(cur[p])
			}
		}
	}
	return func(i, j int) bool {
		return i != j && vc[j][wid[i]] >= ts[i]
	}
}

func isChanOp(op uint8) bool {
	return op == opChanSend || op == opChanRecv || op == opChanClose ||
		op == opChanSendNB || op == opChanRecvNB
}

func isSyncOp(op uint8) bool {
	switch op {
	case opLock, opUnlock, opRLock, opRUnlock, opChanSend, opChanRecv, opChanClose,
		opChanSendNB, opChanRecvNB,
		opSelect, opWaitGroupWait, opWaitGroupAdd, opCondWait, opCondSignal, opCondBroadcast, opOnce,
		opAtomicLoad, opAtomicStore, opAtomicRMW:
		return true
	}
	return false
}

// Explore runs f under distinct schedules using dynamic partial-order reduction
// (DPOR): it explores only one representative of each set of interleavings that
// differ solely in the order of independent operations, reordering just the
// conflicting (same-object, ≥1 write) operations. It stops at the first schedule
// that panics or deadlocks.
//
// Soundness depends on conflicting operations being observable. Under
// -gcflags=-weave every shared memory access is recorded, so data races are
// explored soundly. (Recording of channel/mutex operations for DPOR is a
// separate step; without it, sync-only conflicts are not yet reduced soundly.)
//
// Explore bounds itself with DefaultMaxSchedules so a huge state space cannot
// run forever; use ExploreBudget or ExploreBounded for explicit bounds.
func Explore(f func()) Result { return ExploreBounded(f, DefaultMaxSchedules, -1, 0) }

// DefaultMaxSchedules is the fallback ceiling on how many schedules Explore will
// run before giving up and marking the result Truncated. It is generous enough
// not to trip on ordinary unit tests but bounds pathological state spaces.
const DefaultMaxSchedules = 1_000_000

// ExploreBudget is Explore with an explicit schedule budget (maxSchedules <= 0
// means unlimited). When the budget is reached before the state space is
// exhausted, exploration stops and Result.Truncated is set, so an incomplete
// run is never mistaken for a clean pass.
func ExploreBudget(f func(), maxSchedules int) Result {
	return ExploreBounded(f, maxSchedules, -1, 0)
}

// ExploreBounded is ExploreBudget with context bounding: when maxPreemptions >= 0
// the search is restricted to schedules with at most that many preemptions
// (non-forced context switches — the scheduler leaving a goroutine that was
// still runnable). This is the CHESS insight that most concurrency bugs surface
// with very few preemptions, so bounding finds them while exploring far fewer
// schedules. maxPreemptions < 0 disables the bound.
//
// The bound is complete within itself: every schedule with <= maxPreemptions
// preemptions is still explored (a backtrack is skipped only when its forced
// prefix already exceeds the bound, and every prefix of an in-bound schedule is
// itself in bound). It is not marked Truncated, since "no failure within c
// preemptions" is a real guarantee, not an incomplete search.
//
// maxDuration > 0 stops exploration once that much wall-clock time has elapsed,
// marking the result Truncated. It is a SOFT limit checked between schedules,
// not a hard per-schedule deadline: a single schedule that hangs — an
// uninstrumented pure-compute infinite loop, or a block the controller cannot
// observe — is not interrupted by it (that would require a preemption watchdog
// inside the controller, which is not yet implemented; use an external
// `go test -timeout` as the backstop). It bounds total exploration time for
// models whose individual schedules all terminate.
func ExploreBounded(f func(), maxSchedules, maxPreemptions int, maxDuration time.Duration) Result {
	e := newExplorer(f)

	type frame struct {
		backtrack map[int32]bool
		done      map[int32]bool
		selWid    int32          // selecting participant if this step is a select event
		selBranch int            // number of ready cases (>1 ⇒ enumerable)
		selEvent  int            // this select's event index
		selDone   map[int32]bool // select cases already explored at this step
	}
	var stack []*frame
	var plan, selPlan []int32
	start := time.Now()

	for {
		steps, nsel, outcome, failure, unfired := runSchedule(e.f, plan, e.traceWid, e.traceOp, e.traceValSet, selPlan, e.selTrace, e.selBranch, e.selStepIdx, e.traceAddr, e.traceSize, e.traceEnabled, e.tracePC, e.spawnPC, e.traceVal, maxPreemptions >= 0)
		e.res.Runs++
		if unfired {
			e.res.UnfiredTimer = true
		}

		// The runtime keeps counting scheduling points after the trace buffers
		// overflow (reported as outcome==2 below), so steps/nsel can exceed the
		// buffer length; clamp before any slicing so diagnostics never read out of
		// range and an overflow becomes Truncated rather than a panic.
		if steps > traceCap {
			steps = traceCap
		}
		if nsel > traceCap {
			nsel = traceCap
		}

		wid := e.traceWid[:steps]
		op := e.traceOp[:steps]
		addr := e.traceAddr[:steps]
		size := e.traceSize[:steps]
		en := e.traceEnabled[:steps]
		for _, w := range wid {
			if w == ClockWid {
				e.res.ClockAdvanced = true
				break
			}
		}
		// Before drawing any conclusion from this run, make sure it actually
		// reproduced the prefix it was told to replay.
		if !e.checkPrefix(wid, op, e.tracePC[:steps]) {
			return e.res
		}
		e.checkCarriedState(op, addr, e.tracePC[:steps], e.traceValSet[:steps], e.traceVal[:steps])

		selTrace := e.selTrace[:nsel]
		selBranch := e.selBranch[:nsel]
		selStepIdx := e.selStepIdx[:nsel]

		if failure != nil {
			if e.confirm(selPlan, wid, false); e.res.NotConfirmed {
				return e.res // withhold a counterexample that does not rerun
			}
			e.res.Failed = true
			e.res.Value = failure
			e.res.Seed = encodeSeed(wid, selTrace)
			e.res.Trace = buildTrace(wid, op, addr, e.tracePC[:steps], e.traceValSet[:steps], e.traceVal[:steps])
			e.res.Goroutines = buildGoroutines(wid, e.spawnPC)
			return e.res
		}
		if outcome == 1 {
			if e.confirm(selPlan, wid, true); e.res.NotConfirmed {
				return e.res // withhold a counterexample that does not rerun
			}
			e.res.Deadlock = true
			e.res.Seed = encodeSeed(wid, selTrace)
			e.res.Trace = buildTrace(wid, op, addr, e.tracePC[:steps], e.traceValSet[:steps], e.traceVal[:steps])
			e.res.Goroutines = buildGoroutines(wid, e.spawnPC)
			return e.res
		}
		if outcome == 2 {
			e.res.Truncated = true
			e.res.TruncatedReason = "capacity limit (too many participants or trace recording space)"
			return e.res
		}
		// Reproducibility probe, run once per exploration on a model that came back
		// clean. The prefix check above only covers the transitions a run was told to
		// replay, so behaviour that drifts LATER than the forced choice point would
		// slip through — a package-level sync.Once being the canonical example. Run the
		// default schedule again and require the WHOLE trace to match: same starting
		// state, same choices, therefore same transitions.
		//
		// The first run is deliberately discarded as a warm-up. A model that so much as
		// calls fmt.Sprint or t.Log differs between its first and second run for
		// reasons that have nothing to do with the model: sync.Pool registers itself
		// under a lock on first use and takes an atomic fast path afterwards, and lazy
		// singletons all through the standard library behave the same way. Comparing
		// runs 2 and 3 skips that class entirely while still catching state the model
		// itself accumulates, which keeps drifting on every later run.
		//
		// Skipped when the schedule budget cannot afford three runs, so a budget of one
		// still means exactly one run.
		if e.probe < 2 && (maxSchedules <= 0 || maxSchedules >= 3) {
			e.snapshot(steps, wid, op, e.tracePC[:steps], true, e.probe == 0)
			e.probe++
			continue // rerun with the same plan; the analysis happens once the probe is done
		}

		for len(stack) < steps {
			stack = append(stack, &frame{backtrack: map[int32]bool{}, done: map[int32]bool{}})
		}
		for i := 0; i < steps; i++ {
			stack[i].backtrack[wid[i]] = true
			stack[i].done[wid[i]] = true
		}
		// Mark select events at their steps so the search can enumerate the other
		// ready cases (a second choice dimension; see the deepest-frame pick below).
		for e2 := 0; e2 < nsel; e2++ {
			d2 := int(selStepIdx[e2])
			if d2 < 0 || d2 >= steps {
				continue
			}
			fr := stack[d2]
			fr.selWid = wid[d2]
			fr.selBranch = int(selBranch[e2])
			fr.selEvent = e2
			if fr.selDone == nil {
				fr.selDone = map[int32]bool{}
			}
			fr.selDone[selTrace[e2]] = true
		}

		// Context bounding: cum[i] counts the preemptions (non-forced context
		// switches) among steps 1..i of this run. Forcing participant w at step i
		// yields a schedule whose prefix has cum[i-1] preemptions plus one more if
		// switching to w there is itself a preemption; allow the backtrack only if
		// that stays within maxPreemptions.
		// The synthetic clock is priced differently from a goroutine, in both
		// directions. Leaving it un-advanced is never a preemption (it is not a
		// goroutine we switched away from). Choosing it is one whenever ANY real
		// participant was runnable — not merely when the previous one still was —
		// because letting time move instead of letting a ready goroutine run is
		// exactly the relaxation D22 introduces. That pricing is what makes
		// WEAVE_MAX_PREEMPTIONS=0 an exact opt-out: at 0 the clock can only be chosen
		// when nothing else can run, which is synctest's idle-only contract.
		clockPreempts := func(i int) bool {
			return en[i]&^(uint64(1)<<uint(ClockWid)) != 0
		}
		var cum []int
		if maxPreemptions >= 0 {
			cum = make([]int, steps)
			for i := 1; i < steps; i++ {
				cum[i] = cum[i-1]
				switch {
				case wid[i] == ClockWid:
					if clockPreempts(i) {
						cum[i]++
					}
				case wid[i] != wid[i-1] && wid[i-1] != ClockWid && en[i]&(1<<uint(wid[i-1])) != 0:
					cum[i]++
				}
			}
		}
		allow := func(i int, w int32) bool {
			if maxPreemptions < 0 {
				return true
			}
			cost := 0
			if i >= 1 {
				cost = cum[i-1]
			}
			switch {
			case w == ClockWid:
				if clockPreempts(i) {
					cost++
				}
			case i >= 1 && w != wid[i-1] && wid[i-1] != ClockWid && en[i]&(1<<uint(wid[i-1])) != 0:
				cost++
			}
			return cost <= maxPreemptions
		}

		// The clock is an OPTIONAL transition: unlike a participant, which always
		// runs eventually and so always shows up in some run, the clock may never
		// appear at all — the default policy picks it only when nothing else can run.
		// DPOR's backward analysis can only reverse transitions it has observed, so
		// without seeding it here "the timeout fired" would never be explored. Add a
		// backtrack wherever the clock was offered but not taken; done/allow keep it
		// bounded, and conflict() reduces the resulting positions.
		for i := 0; i < steps; i++ {
			if en[i]&(1<<uint(ClockWid)) == 0 || wid[i] == ClockWid {
				continue
			}
			if !stack[i].done[ClockWid] && allow(i, ClockWid) {
				stack[i].backtrack[ClockWid] = true
			}
		}

		hb := channelHB(wid, op, addr, steps)

		// Backward analysis: for each transition j, find the nearest earlier
		// concurrent transition i it conflicts with, and schedule the reversal
		// (run j's participant at state i) in a future run.
		for j := 0; j < steps; j++ {
			for i := j - 1; i >= 0; i-- {
				if wid[i] == wid[j] {
					continue // program-order predecessor of j (happens-before j)
				}
				if conflict(uint8(op[i]), addr[i], size[i], uint8(op[j]), addr[j], size[j]) {
					if hb(i, j) {
						// i happens-before j (e.g. ordered through a channel), so the
						// pair is not a reversible race; keep scanning for an earlier
						// concurrent conflict rather than stopping here.
						continue
					}
					wj := wid[j]
					if en[i]&(1<<uint(wj)) != 0 {
						if !stack[i].done[wj] && allow(i, wj) {
							stack[i].backtrack[wj] = true
						}
					} else {
						// j's participant was not runnable at state i; explore all
						// runnable participants there to reach the reversal.
						for w := int32(0); w < 64; w++ {
							if en[i]&(1<<uint(w)) != 0 && !stack[i].done[w] && allow(i, w) {
								stack[i].backtrack[w] = true
							}
						}
					}
					break // nearest conflicting transition only
				}
			}
		}

		// Pick the deepest frame with an unexplored alternative: either a different
		// participant to run (wid backtrack) or, at a select event, a different
		// ready case to fire (select-case backtrack). Case enumeration is exhaustive
		// and composes with the wid-level DPOR.
		d, pick := -1, int32(-1)
		selD, selCase := -1, int32(-1)
		for i := len(stack) - 1; i >= 0; i-- {
			for w := range stack[i].backtrack {
				if !stack[i].done[w] && (pick == -1 || w < pick) {
					pick = w
				}
			}
			if pick != -1 {
				d = i
				break
			}
			if stack[i].selBranch > 1 {
				for c := int32(0); c < int32(stack[i].selBranch); c++ {
					if !stack[i].selDone[c] {
						selCase = c
						break
					}
				}
				if selCase != -1 {
					selD = i
					break
				}
			}
		}
		// Apply the schedule/time budgets only once we know another schedule
		// actually remains to run; otherwise a budget that exactly covers the whole
		// state space would falsely mark a fully-explored model as Truncated.
		if d >= 0 || selD >= 0 {
			if maxSchedules > 0 && e.res.Runs >= maxSchedules {
				e.res.Truncated = true
				e.res.TruncatedReason = "schedule budget reached"
				return e.res
			}
			if maxDuration > 0 && time.Since(start) >= maxDuration {
				e.res.Truncated = true
				e.res.TruncatedReason = "time budget reached"
				return e.res
			}
		}
		switch {
		case d >= 0:
			// wid backtrack: rerun with a different participant at step d. Force the
			// select choices for events strictly before step d so the prefix repeats.
			stack[d].done[pick] = true
			plan = append(plan[:0], wid[:d]...)
			plan = append(plan, pick)
			cnt := 0
			for cnt < len(selStepIdx) && int(selStepIdx[cnt]) < d {
				cnt++
			}
			selPlan = append(selPlan[:0], selTrace[:cnt]...)
			stack = stack[:d+1] // discard the now-stale subtree below d
			e.snapshotPrefix(d, wid, op, e.tracePC[:steps])
		case selD >= 0:
			// select-case backtrack: rerun the same prefix but fire a different ready
			// case at this select. Force the same participant at step selD and the
			// recorded case choices up to this select, then the new case.
			fr := stack[selD]
			fr.selDone[selCase] = true
			plan = append(plan[:0], wid[:selD]...)
			plan = append(plan, fr.selWid)
			selPlan = append(selPlan[:0], selTrace[:fr.selEvent]...)
			selPlan = append(selPlan, selCase)
			stack = stack[:selD+1]
			e.snapshotPrefix(selD, wid, op, e.tracePC[:steps])
		default:
			return e.res
		}
	}
}

// confirm reruns the schedule that just failed, with the identical choice vector,
// and records whether the failure happened again. It is the last thing weave does
// before handing a counterexample to the reader: a reported interleaving that does
// not reproduce is either an artifact of state left behind by an earlier schedule or
// a bug in weave, and in both cases saying so beats printing a trace the reader
// cannot act on.
//
// Only one extra run, and only when something is actually being reported.
func (e *explorer) confirm(selPlan, wid []int32, wantDeadlock bool) {
	// Force the whole schedule, not just its prefix: the choices past the backtrack
	// point were made by the default policy, and replaying them explicitly is what
	// makes this a rerun of the same interleaving rather than of the same prefix.
	full := append([]int32(nil), wid...)
	sel := append([]int32(nil), selPlan...)
	c := newExplorer(e.f)
	_, _, outcome, failure, _ := runSchedule(c.f, full, c.traceWid, c.traceOp, c.traceValSet, sel,
		c.selTrace, c.selBranch, c.selStepIdx, c.traceAddr, c.traceSize, c.traceEnabled, c.tracePC,
		c.spawnPC, c.traceVal, false)
	e.res.Runs++
	again := failure != nil
	if wantDeadlock {
		again = outcome == 1
	}
	if again {
		return
	}
	what := "the panic did not happen again"
	if wantDeadlock {
		what = "the deadlock did not happen again"
	}
	e.res.NotConfirmed = true
	e.res.NotConfirmedReason = what + " when the same schedule was rerun"
}

// checkCarriedState looks for a value that flowed from one schedule into the next.
//
// The interesting event is not that the model WROTE something outliving a schedule —
// collecting results in an outer slice or bumping a counter is normal, and complaining
// about it would rule out most coarse-grained tests. It is that the model READ a
// location it had not yet written in this run, and got a different value than the
// first run read there. That is precisely "this schedule's behaviour can depend on
// what an earlier schedule did", and nothing else trips it:
//
//   - a location the model only ever writes is never read first, so it never appears;
//   - a location nobody writes reads the same value every run;
//   - freshly allocated memory is zero-initialized, so a reused address reads 0 in
//     both runs — which is what makes this safe without tracking allocations. Only
//     memory that outlives a schedule can read differently, and closure-captured
//     variables qualify: they are allocated once, kept alive by the closure, and Go's
//     collector does not move them, so their addresses are stable across runs.
//
// This is evidence for other reports to cite, never a failure of its own (see
// Result.CarriedState). Requires -weave: without memory instrumentation there are no
// read/write transitions to inspect.
func (e *explorer) checkCarriedState(op []int32, addr []int64, pc []uint64, valSet []int32, val []uint64) {
	if e.res.CarriedState {
		return // one piece of evidence is enough; the first is the most relevant
	}
	written := make(map[int64]bool, 8)
	first := e.firstRead
	if first == nil {
		first = map[int64]readObs{}
		e.firstRead = first
	}
	for i := range op {
		a := addr[i]
		if a == 0 || valSet[i] == 0 {
			continue
		}
		switch uint8(op[i]) {
		case opWrite:
			written[a] = true
			e.lastWrite[a] = pc[i]
		case opRead:
			if written[a] {
				continue // this run established the value itself
			}
			if val[i] >= carriedValueLimit {
				// Only small scalars are usable evidence. A pointer-sized value differs
				// between runs because it points at a freshly allocated object, not
				// because any state was carried over — comparing those would report every
				// model that reads a pointer it did not just write.
				continue
			}
			obs, seen := first[a]
			if !seen {
				first[a] = readObs{val: val[i], pc: pc[i]}
				continue
			}
			if obs.val == val[i] || obs.val >= carriedValueLimit {
				continue
			}
			b := []byte("read ")
			b = appendInt(b, int(val[i]))
			b = appendAt(b, pc[i])
			b = append(b, ", but the first schedule read "...)
			b = appendInt(b, int(obs.val))
			b = appendAt(b, obs.pc)
			if w, ok := e.lastWrite[a]; ok && w != 0 {
				b = append(b, "; an earlier schedule wrote it"...)
				b = appendAt(b, w)
			}
			e.res.CarriedState = true
			e.res.CarriedStateReason = string(b)
			return
		}
	}
}

// readObs is the first value a location was seen to hold before this exploration
// wrote it, and where that read happened.
type readObs struct {
	val uint64
	pc  uint64
}

// appendAt appends " at file:line" for a PC, or nothing if it has no position.
func appendAt(b []byte, pc uint64) []byte {
	if pc == 0 {
		return b
	}
	fr, _ := runtime.CallersFrames([]uintptr{uintptr(pc)}).Next()
	if fr.File == "" {
		return b
	}
	b = append(b, " at "...)
	b = append(b, baseName(fr.File)...)
	b = append(b, ':')
	return appendInt(b, fr.Line)
}

// carriedValueLimit bounds the values checkCarriedState is willing to reason about.
// Counters, flags, lengths and small ids live well below it; anything above is a
// pointer, a hash or a nanosecond timestamp, none of which say anything about state
// crossing a schedule boundary. It also keeps appendInt within its digit budget.
const carriedValueLimit = 1 << 32

// snapshotPrefix records the transitions the next run must reproduce: everything
// strictly before the step whose choice we are about to change. Step d itself is
// deliberately excluded — that is the new choice, so it is expected to differ.
func (e *explorer) snapshotPrefix(d int, wid, op []int32, pc []uint64) {
	e.snapshot(d, wid, op, pc, false, false)
}

// snapshot records the transitions the next run must reproduce. full says the
// snapshot covers a whole run, in which case the next run must also END there; a
// prefix snapshot only constrains its own first d transitions, since the run
// legitimately continues past them.
func (e *explorer) snapshot(d int, wid, op []int32, pc []uint64, full, soft bool) {
	e.expectWid = append(e.expectWid[:0], wid[:d]...)
	e.expectOp = append(e.expectOp[:0], op[:d]...)
	e.expectPC = append(e.expectPC[:0], pc[:d]...)
	e.expectValid = true
	e.expectFull = full
	e.expectSoft = soft
}

// checkPrefix verifies that a forced prefix replayed to the same transitions. A
// mismatch means the model is not reproducible: replaying the same choices from the
// same starting state produced different behaviour, so every conclusion the search
// draws is meaningless. The usual cause is state that outlives one schedule (a
// package-level sync.Once or cache, or a variable declared outside the closure), but
// any nondeterminism the scheduler does not control does it too — rand, real time,
// an un-determinized map iteration, behaviour that depends on an address.
//
// Only wid and op are compared. Addresses and values legitimately differ between
// runs (the heap layout moves), which is exactly why divergence has to be detected
// at the level of "which participant did what kind of operation".
func (e *explorer) checkPrefix(wid, op []int32, pc []uint64) bool {
	if !e.expectValid {
		return true
	}
	e.expectValid = false
	soft := e.expectSoft
	e.expectSoft = false
	report := func(reason string) bool {
		if soft {
			e.res.WarmupDiverged = true
			e.res.WarmupDivergedReason = reason
			return true // exploration continues on the post-warm-up model
		}
		e.res.Diverged = true
		e.res.DivergedReason = reason
		return false
	}
	// Compare the transitions they have in common first: that pinpoints the step, and
	// is more useful than "the trace got shorter". Only if every common transition
	// matches is a length difference the signal — which is the shape a skipped
	// initializer takes (the same operations happen, there are just fewer of them).
	if len(wid) < len(e.expectWid) && !e.expectFull {
		// The run ended before it finished replaying the prefix it was given.
		e.expectFull = true // reuse the length branch below for the message
	}
	n := min(len(wid), len(e.expectWid))
	for i := 0; i < n; i++ {
		// Only the participant sequence is compared, not the operations. DPOR forces
		// wids, so a wid mismatch means the replay did not happen; whereas "same
		// goroutine, different operation" is dominated by library warm-up that has
		// nothing to do with the model — sync.Pool taking a lock on first use and an
		// atomic afterwards, a lazily grown output buffer inside testing. Flagging
		// those would make weave unusable for any model that calls fmt.Sprint or t.Log.
		if wid[i] == e.expectWid[i] {
			continue
		}
		b := []byte("diverged at step ")
		b = appendInt(b, i+1)
		b = append(b, ": expected "...)
		b = appendStep(b, e.expectWid[i], e.expectOp[i], e.expectPC[i])
		b = append(b, ", got "...)
		b = appendStep(b, wid[i], op[i], pc[i])
		return report(string(b))
	}
	if e.expectFull && len(wid) != len(e.expectWid) {
		b := []byte("diverged at step ")
		b = appendInt(b, n+1)
		b = append(b, ": the same choices produced "...)
		b = appendInt(b, len(wid))
		b = append(b, " transition(s) instead of "...)
		b = appendInt(b, len(e.expectWid))
		if len(wid) < len(e.expectWid) {
			b = append(b, " — it stopped short of "...)
			b = appendStep(b, e.expectWid[n], e.expectOp[n], e.expectPC[n])
		} else {
			b = append(b, " — it continued with "...)
			b = appendStep(b, wid[n], op[n], pc[n])
		}
		return report(string(b))
	}
	return true
}

// appendStep renders one transition for a divergence message, e.g.
// "g1 once (cache.go:18)". Formatted by hand to keep this package's dependencies at
// runtime+time (see the seed encoder, which does the same).
func appendStep(b []byte, wid, op int32, pc uint64) []byte {
	if wid == ClockWid {
		b = append(b, "clock"...)
	} else {
		b = append(b, 'g')
		b = appendInt(b, int(wid))
	}
	b = append(b, ' ')
	b = append(b, opName(uint8(op))...)
	if pc != 0 {
		if fr, _ := runtime.CallersFrames([]uintptr{uintptr(pc)}).Next(); fr.File != "" {
			b = append(b, " ("...)
			b = append(b, baseName(fr.File)...)
			b = append(b, ':')
			b = appendInt(b, fr.Line)
			b = append(b, ')')
		}
	}
	return b
}

// baseName is filepath.Base for the forward-slash paths runtime reports, avoiding a
// path/filepath dependency.
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// Replay deterministically re-runs the single interleaving encoded by seed (as
// produced in Result.Seed), so a discovered failure can be reproduced.
func Replay(seed string, f func()) Result {
	e := newExplorer(f)
	plan, selPlan := decodeSeed(seed)
	steps, _, outcome, failure, unfired := runSchedule(e.f, plan, e.traceWid, e.traceOp, e.traceValSet, selPlan, e.selTrace, e.selBranch, e.selStepIdx, e.traceAddr, e.traceSize, e.traceEnabled, e.tracePC, e.spawnPC, e.traceVal, false)
	e.res.UnfiredTimer = unfired
	e.res.Runs = 1
	for _, w := range e.traceWid[:min(steps, traceCap)] {
		if w == ClockWid {
			e.res.ClockAdvanced = true
			break
		}
	}
	// A seed is a vector of forced choices; if the run did not follow it, the seed
	// does not describe this model (a stale seed, or a model that is not
	// reproducible). weaveChoose falls back to the lowest runnable wid in that case,
	// which used to make the mismatch silent — and a silent fallback reports
	// "replayed, no failure", the most misleading answer possible.
	if n := min(len(plan), min(steps, traceCap)); n > 0 {
		e.expectWid = append(e.expectWid[:0], plan[:n]...)
		e.expectOp = append(e.expectOp[:0], e.traceOp[:n]...) // ops are not forced; compare wid only
		e.expectPC = append(e.expectPC[:0], e.tracePC[:n]...)
		e.expectValid = true
		e.expectSoft = false
		e.checkPrefix(e.traceWid[:n], e.traceOp[:n], e.tracePC[:n])
	}
	e.res.Seed = seed
	// Same clamp/outcome ordering as ExploreBounded: the runtime keeps counting
	// scheduling points after the trace buffers overflow, so clamp before slicing
	// (never read out of range) and report Truncated for a capacity overflow
	// rather than panicking.
	if steps > traceCap {
		steps = traceCap
	}
	if outcome == 2 {
		e.res.Truncated = true
		e.res.TruncatedReason = "capacity limit (too many participants or trace recording space)"
	}
	e.res.Trace = buildTrace(e.traceWid[:steps], e.traceOp[:steps], e.traceAddr[:steps], e.tracePC[:steps], e.traceValSet[:steps], e.traceVal[:steps])
	e.res.Goroutines = buildGoroutines(e.traceWid[:steps], e.spawnPC)
	if failure != nil {
		e.res.Failed = true
		e.res.Value = failure
	}
	if outcome == 1 {
		e.res.Deadlock = true
	}
	return e.res
}

func buildTrace(wid, op []int32, addr []int64, pc []uint64, valSet []int32, val []uint64) []Step {
	t := make([]Step, len(wid))
	for i := range wid {
		s := Step{Wid: int(wid[i]), Op: opName(uint8(op[i])), Addr: uint64(addr[i])}
		if pc[i] != 0 {
			fr, _ := runtime.CallersFrames([]uintptr{uintptr(pc[i])}).Next()
			s.File, s.Line = fr.File, fr.Line
		}
		if valSet[i] != 0 {
			s.Val, s.HasVal = val[i], true
		}
		t[i] = s
	}
	return t
}

// buildGoroutines resolves the creation site of every participant that appears
// in the interleaving. spawnPC is indexed by wid; slot 0 (the model root) has no
// meaningful go statement and is reported with an empty location.
func buildGoroutines(wid []int32, spawnPC []uint64) []Goroutine {
	max := 0
	for _, w := range wid {
		if w == ClockWid {
			continue // the synthetic clock is not a goroutine
		}
		if int(w) > max {
			max = int(w)
		}
	}
	gs := make([]Goroutine, 0, max+1)
	for w := 0; w <= max; w++ {
		g := Goroutine{Wid: w}
		if w != 0 && w < len(spawnPC) && spawnPC[w] != 0 {
			fr, _ := runtime.CallersFrames([]uintptr{uintptr(spawnPC[w])}).Next()
			g.File, g.Line, g.Func = fr.File, fr.Line, fr.Function
		}
		gs = append(gs, g)
	}
	return gs
}

func opName(op uint8) string {
	switch op {
	case opRead:
		return "read"
	case opWrite:
		return "write"
	case opLock:
		return "lock"
	case opUnlock:
		return "unlock"
	case opRLock:
		return "rlock"
	case opRUnlock:
		return "runlock"
	case opClockAdvance:
		return "clock advance"
	case opChanSend:
		return "chan send"
	case opChanRecv:
		return "chan recv"
	case opChanClose:
		return "chan close"
	case opChanSendNB:
		return "chan send (nb)"
	case opChanRecvNB:
		return "chan recv (nb)"
	case opSelect:
		return "select"
	case opWaitGroupWait:
		return "wg wait"
	case opWaitGroupAdd:
		return "wg add"
	case opCondWait:
		return "cond wait"
	case opCondSignal:
		return "cond signal"
	case opCondBroadcast:
		return "cond broadcast"
	case opOnce:
		return "once"
	case opAtomicLoad:
		return "atomic load"
	case opAtomicStore:
		return "atomic store"
	case opAtomicRMW:
		return "atomic rmw"
	}
	return "run"
}

// encodeSeed encodes the wid schedule and, after a '|', the select-case choices,
// so Replay can reproduce a select-dependent failure. The select part is omitted
// when there were no select events.
func encodeSeed(wid, sel []int32) string {
	s := encodeInts(wid)
	if len(sel) > 0 {
		s += "|" + encodeInts(sel)
	}
	return s
}

func encodeInts(xs []int32) string {
	if len(xs) == 0 {
		return ""
	}
	b := make([]byte, 0, len(xs)*3)
	for i, x := range xs {
		if i > 0 {
			b = append(b, '.')
		}
		b = appendInt(b, int(x))
	}
	return string(b)
}

func decodeSeed(seed string) (wid, sel []int32) {
	widPart, selPart := seed, ""
	for i := 0; i < len(seed); i++ {
		if seed[i] == '|' {
			widPart, selPart = seed[:i], seed[i+1:]
			break
		}
	}
	return decodeInts(widPart), decodeInts(selPart)
}

func decodeInts(s string) []int32 {
	if s == "" {
		return nil
	}
	var out []int32
	n, has := 0, false
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
			has = true
		} else {
			if has {
				out = append(out, int32(n))
			}
			n, has = 0, false
		}
	}
	return out
}

func appendInt(b []byte, n int) []byte {
	if n == 0 {
		return append(b, '0')
	}
	var tmp [12]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[i:]...)
}

// exploreExhaustive enumerates every interleaving with no reduction. Used to
// validate that DPOR finds the same outcomes while exploring no more schedules.
func exploreExhaustive(f func()) Result {
	e := newExplorer(f)
	var plan []int32
	for {
		steps, _, outcome, failure, _ := runSchedule(e.f, plan, e.traceWid, e.traceOp, e.traceValSet, nil, nil, nil, nil, e.traceAddr, e.traceSize, e.traceEnabled, e.tracePC, e.spawnPC, e.traceVal, false)
		e.res.Runs++
		if failure != nil {
			e.res.Failed = true
			e.res.Value = failure
			return e.res
		}
		if outcome == 1 {
			e.res.Deadlock = true
			return e.res
		}
		if outcome == 2 {
			return e.res
		}
		plan = nextExhaustive(e.traceWid[:steps], e.traceEnabled[:steps])
		if plan == nil {
			return e.res
		}
	}
}

// nextExhaustive advances an odometer over the runnable participants: at the
// right-most step where a higher-numbered runnable participant exists, switch to
// it and drop the suffix.
func nextExhaustive(wid []int32, en []uint64) []int32 {
	for i := len(wid) - 1; i >= 0; i-- {
		for w := wid[i] + 1; w < 64; w++ {
			if en[i]&(1<<uint(w)) != 0 {
				np := make([]int32, i+1)
				copy(np, wid[:i])
				np[i] = w
				return np
			}
		}
	}
	return nil
}
