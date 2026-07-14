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
)

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

	// Truncated is set when exploration stopped before the state space was
	// exhausted (schedule budget reached, or a capacity limit hit). A truncated
	// run that found no failure is inconclusive, not a clean pass; callers must
	// not report it as success. TruncatedReason gives a short explanation.
	Truncated       bool
	TruncatedReason string
}

// Run executes f once in a controlled bubble (a single default schedule).
// It panics if the run deadlocks.
func Run(f func()) {
	_, _, outcome, failure := runSchedule(f, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false)
	if failure != nil {
		panic(failure)
	}
	if outcome == 1 {
		panic("weave: deadlock: all goroutines in bubble are blocked")
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
}

func newExplorer(f func()) *explorer {
	return &explorer{
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
	tainted := map[int64]bool{}
	hasSelect := false
	for i := 0; i < steps; i++ {
		switch uint8(op[i]) {
		case opChanSendNB, opChanRecvNB:
			tainted[addr[i]] = true
		case opSelect:
			hasSelect = true
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
	case opLock, opUnlock, opChanSend, opChanRecv, opChanClose, opChanSendNB, opChanRecvNB,
		opSelect, opWaitGroupWait, opWaitGroupAdd, opCondWait, opCondSignal, opCondBroadcast, opOnce:
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
		steps, nsel, outcome, failure := runSchedule(e.f, plan, e.traceWid, e.traceOp, e.traceValSet, selPlan, e.selTrace, e.selBranch, e.selStepIdx, e.traceAddr, e.traceSize, e.traceEnabled, e.tracePC, e.spawnPC, e.traceVal, maxPreemptions >= 0)
		e.res.Runs++

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
		selTrace := e.selTrace[:nsel]
		selBranch := e.selBranch[:nsel]
		selStepIdx := e.selStepIdx[:nsel]

		if failure != nil {
			e.res.Failed = true
			e.res.Value = failure
			e.res.Seed = encodeSeed(wid, selTrace)
			e.res.Trace = buildTrace(wid, op, addr, e.tracePC[:steps], e.traceValSet[:steps], e.traceVal[:steps])
			e.res.Goroutines = buildGoroutines(wid, e.spawnPC)
			return e.res
		}
		if outcome == 1 {
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
		var cum []int
		if maxPreemptions >= 0 {
			cum = make([]int, steps)
			for i := 1; i < steps; i++ {
				cum[i] = cum[i-1]
				if wid[i] != wid[i-1] && en[i]&(1<<uint(wid[i-1])) != 0 {
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
				if w != wid[i-1] && en[i]&(1<<uint(wid[i-1])) != 0 {
					cost++
				}
			}
			return cost <= maxPreemptions
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
		default:
			return e.res
		}
	}
}

// Replay deterministically re-runs the single interleaving encoded by seed (as
// produced in Result.Seed), so a discovered failure can be reproduced.
func Replay(seed string, f func()) Result {
	e := newExplorer(f)
	plan, selPlan := decodeSeed(seed)
	steps, _, outcome, failure := runSchedule(e.f, plan, e.traceWid, e.traceOp, e.traceValSet, selPlan, e.selTrace, e.selBranch, e.selStepIdx, e.traceAddr, e.traceSize, e.traceEnabled, e.tracePC, e.spawnPC, e.traceVal, false)
	e.res.Runs = 1
	e.res.Seed = seed
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
		steps, _, outcome, failure := runSchedule(e.f, plan, e.traceWid, e.traceOp, e.traceValSet, nil, nil, nil, nil, e.traceAddr, e.traceSize, e.traceEnabled, e.tracePC, e.spawnPC, e.traceVal, false)
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
