// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// weave provides a controlled scheduler for synctest bubbles: it serializes the
// bubble's goroutines (one runs at a time) and, at each scheduling point, an
// exploration policy chooses which enabled goroutine runs next. Across many runs
// of the same model, weave systematically enumerates the distinct interleavings
// (currently an exhaustive DFS; DPOR is the planned refinement), so a test can
// discover schedule-dependent bugs (deadlocks today; assertion failures once
// panic capture lands).
//
// weave is always compiled in (no build tag) and runs under a plain `go test`.
// Every hook is gated by a cheap "gp.bubble != nil && controlled" check, the
// same footprint as synctest; ordinary programs pay only a predictable
// not-taken branch on the park/ready/newproc paths.
//
// The token is passed at central points so no per-primitive code is needed:
//   - ready (a sync op wakes a participant): the participant is captured into
//     the runnable set instead of being made OS-runnable (weaveEnqueue).
//   - park_m (a participant blocks on a real sync op): it hands the token to the
//     chosen runnable participant (weaveOnBlock); if none, that is a deadlock.
//   - goroutine exit and explicit yield hand the token off similarly.
//
// Because every real block goes through gopark and every wake through goready,
// real channels, sync.Mutex, sync.WaitGroup, etc. all work with no wrappers.
package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// weaveMaxRunnable bounds the number of simultaneously-runnable participants
// (those waiting for the run token). Ample for unit tests.
const weaveMaxRunnable = 256

// weaveGloballyActive is nonzero while any controlled bubble exists. It lets hot
// user-facing paths (notably sync.Mutex, via internal/sync) skip the weave hook
// with a single global load when weave is not in use, keeping non-weave programs
// cheap without a build flag. The //go:linkname makes it accessible to
// internal/sync.
//
//go:linkname weaveGloballyActive
var weaveGloballyActive uint32

// weaveControl is the run-token scheduler state for a controlled bubble.
// All fields are protected by the owning bubble's mu.
//
// The runnable set is stored in parallel arrays (guintptr + wid/op/addr) so that
// enqueuing needs no write barrier (weaveEnqueue is reached from ready, which
// can run where write barriers are prohibited, e.g. GC credit flushing).
// runnableWid[i] is the stable participant id; runnableOp/Addr[i] is the
// operation that participant is about to perform (its pending transition), or
// weaveOpNone/0 if not yet known (freshly spawned or just woken).
type weaveControl struct {
	runnable       [weaveMaxRunnable]guintptr
	runnableWid    [weaveMaxRunnable]int32
	runnableOp     [weaveMaxRunnable]uint8
	runnableAddr   [weaveMaxRunnable]uintptr
	runnablePC     [weaveMaxRunnable]uintptr // pending memory op's source PC
	runnableVal    [weaveMaxRunnable]uint64  // pending memory op's scalar value
	runnableValSet [weaveMaxRunnable]bool    // whether runnableVal is meaningful
	runnableSize   [weaveMaxRunnable]uintptr // pending memory op's access width (bytes); 0 for non-memory ops
	nrun           int                       // number of valid entries

	nextWid      int32  // next participant id to assign
	live         int    // participants enrolled and not yet exited
	done         bool   // all participants have exited
	deadlock     bool   // some remain but none can run
	advanceClock bool   // no participant is runnable but a timer is pending; root must advance the fake clock
	rootParked   bool   // root goroutine is parked in weaveRootWait
	waiter       *g     // participant parked in weave.Wait, or nil
	randState    uint64 // deterministic RNG state for this run
	panicked     bool   // a participant panicked
	panicValue   any    // the recovered panic value

	// Exploration state. plan forces, at scheduling point i, the wid to run
	// (plan[i]); beyond len(plan) the lowest runnable wid is chosen. The trace
	// buffers record, per step, the wid that ran, its operation and address, and
	// a bitmask of the wids that were runnable, so the driver can run DPOR. All
	// are nil for a plain single-schedule Run.
	plan         []int32
	traceWid     []int32
	traceOp      []int32
	traceAddr    []int64
	traceEnabled []uint64
	tracePC      []uint64 // caller PC of each step's operation (0 if unknown)
	spawnPC      []uint64 // per-wid go-statement PC (creation site), indexed by wid
	traceVal     []uint64 // scalar value observed at each memory op (see traceValSet)
	traceValSet  []int32  // 1 if traceVal[step] holds a meaningful value, else 0
	traceSize    []int64  // memory op's access width (bytes); 0 for non-memory ops
	step         int
	overflow     bool // ran out of recording space or too many participants for DPOR

	// stickyDefault, when set, makes the default choice (beyond the forced plan)
	// continue the previously-run participant if it is still runnable, instead of
	// picking the lowest wid. This keeps unforced suffixes preemption-free, so a
	// context-bounded search (maxPreemptions) never executes a schedule with more
	// preemptions than its forced prefix already accounts for. lastWid is the
	// participant chosen at the previous scheduling point (-1 before the first).
	stickyDefault bool
	lastWid       int32

	// Select-case exploration. A controlled select with several ready cases is a
	// second choice dimension on top of "which participant runs": selPlan forces,
	// at the k-th select event, which ready case fires (beyond len(selPlan) the
	// first ready case is chosen). selTrace/selBranch record, per select event,
	// the case chosen and how many were ready, so the driver can enumerate them.
	selPlan    []int32
	selTrace   []int32
	selBranch  []int32
	selStepIdx []int32 // scheduling-step index of each select event
	selStep    int
}

// weaveRand returns a deterministic pseudo-random value (splitmix64) for a
// controlled bubble, so map iteration order, maphash seeds, etc. are reproducible
// across runs. Only the single running participant calls it (serialized, root
// excluded), so ctl.randState needs no lock.
func weaveRand(b *synctestBubble) uint64 {
	ctl := b.weaveCtl
	ctl.randState += 0x9e3779b97f4a7c15
	z := ctl.randState
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// weaveActive reports whether the current goroutine is a participant in a
// controlled bubble.
//
// The weaveGloballyActive short-circuit keeps the cost on hot paths that call
// this in every program (chan.go, select.go) to a single global load when no
// weave bubble exists anywhere — no getg, no per-goroutine field access — so
// non-weave programs pay essentially nothing.
//
//go:nosplit
func weaveActive() bool {
	gp := getg()
	return gp.bubble != nil && gp.bubble.controlled
}

// weaveControlledParticipant reports whether gp is a schedulable participant of
// a controlled bubble (controlled and not the root/driver goroutine).
//
//go:nosplit
func weaveControlledParticipant(gp *g) bool {
	b := gp.bubble
	return b != nil && b.controlled && gp != b.root
}

// weavePush appends a participant to the runnable set with its pending
// operation, and for a memory op its source PC and observed scalar value.
// Caller holds bubble.mu.
func weavePush(ctl *weaveControl, gp *g, op weaveOp, addr, size, pc uintptr, val uint64, valSet bool) {
	if ctl.nrun >= weaveMaxRunnable {
		// Exceeded the runnable capacity: abort this schedule as a capacity
		// overflow (reported as outcome 2 / Truncated) instead of killing the whole
		// test process with a runtime fatal. The dropped participant stays parked;
		// the run winds down and the driver reports the truncation.
		ctl.overflow = true
		return
	}
	i := ctl.nrun
	ctl.runnable[i].set(gp)
	ctl.runnableWid[i] = gp.weaveWid
	ctl.runnableOp[i] = uint8(op)
	ctl.runnableAddr[i] = addr
	ctl.runnableSize[i] = size
	ctl.runnablePC[i] = pc
	ctl.runnableVal[i] = val
	ctl.runnableValSet[i] = valSet
	ctl.nrun++
}

// weaveChoose returns the index in the runnable set of the participant to run
// next, replaying the forced plan (by wid) and recording the transition into the
// trace buffers. Caller holds bubble.mu.
func weaveChoose(ctl *weaveControl) int {
	// Determine the target: the planned wid, or the lowest wid by default.
	idx := 0
	if ctl.step < len(ctl.plan) {
		target := ctl.plan[ctl.step]
		idx = -1
		for i := 0; i < ctl.nrun; i++ {
			if ctl.runnableWid[i] == target {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = weaveLowestWid(ctl) // planned wid not runnable; fall back
		}
	} else {
		idx = weaveDefaultChoice(ctl)
	}

	// Record the transition.
	if ctl.traceWid != nil {
		if ctl.step < len(ctl.traceWid) {
			var enabled uint64
			for i := 0; i < ctl.nrun; i++ {
				if w := ctl.runnableWid[i]; w < 64 {
					enabled |= 1 << uint(w)
				}
			}
			ctl.traceWid[ctl.step] = ctl.runnableWid[idx]
			ctl.traceOp[ctl.step] = int32(ctl.runnableOp[idx])
			ctl.traceAddr[ctl.step] = int64(ctl.runnableAddr[idx])
			ctl.traceEnabled[ctl.step] = enabled
			if ctl.traceSize != nil {
				ctl.traceSize[ctl.step] = int64(ctl.runnableSize[idx])
			}
			// A source PC is only meaningful for memory read/write ops; other
			// transitions (run/exit/chan/mutex) reuse the g and would show a
			// stale PC.
			if o := ctl.runnableOp[idx]; o == uint8(weaveOpRead) || o == uint8(weaveOpWrite) {
				ctl.tracePC[ctl.step] = uint64(ctl.runnablePC[idx])
				if ctl.traceVal != nil {
					// Record the value the compiler supplied (writes) or a placeholder
					// (reads). A read's value is backfilled by weaveread after the
					// participant resumes and performs the load, so the runtime never
					// dereferences a user address from the controller context (where a
					// fault would be a process-fatal rather than a recoverable panic).
					ctl.traceVal[ctl.step] = ctl.runnableVal[idx]
					if ctl.runnableValSet[idx] {
						ctl.traceValSet[ctl.step] = 1
					} else {
						ctl.traceValSet[ctl.step] = 0
					}
				}
			} else {
				ctl.tracePC[ctl.step] = 0
				if ctl.traceVal != nil {
					ctl.traceVal[ctl.step] = 0
					ctl.traceValSet[ctl.step] = 0
				}
			}
		} else {
			ctl.overflow = true
		}
	}
	ctl.lastWid = ctl.runnableWid[idx]
	ctl.step++
	return idx
}

// weaveDefaultChoice picks the next participant when the run is past its forced
// plan. With stickyDefault it continues the previously-run participant if it is
// still runnable (a preemption-free choice); otherwise it picks the lowest wid.
func weaveDefaultChoice(ctl *weaveControl) int {
	if ctl.stickyDefault && ctl.lastWid >= 0 {
		for i := 0; i < ctl.nrun; i++ {
			if ctl.runnableWid[i] == ctl.lastWid {
				return i
			}
		}
	}
	return weaveLowestWid(ctl)
}

func weaveLowestWid(ctl *weaveControl) int {
	idx := 0
	for i := 1; i < ctl.nrun; i++ {
		if ctl.runnableWid[i] < ctl.runnableWid[idx] {
			idx = i
		}
	}
	return idx
}

// weaveTake chooses and removes the next participant to run, or nil if none are
// runnable. Caller holds bubble.mu.
func weaveTake(ctl *weaveControl) *g {
	if ctl.nrun == 0 {
		return nil
	}
	k := weaveChoose(ctl)
	gp := ctl.runnable[k].ptr()
	// Shift the tail down to remove index k, preserving relative order.
	for i := k; i < ctl.nrun-1; i++ {
		ctl.runnable[i] = ctl.runnable[i+1]
		ctl.runnableWid[i] = ctl.runnableWid[i+1]
		ctl.runnableOp[i] = ctl.runnableOp[i+1]
		ctl.runnableAddr[i] = ctl.runnableAddr[i+1]
		ctl.runnablePC[i] = ctl.runnablePC[i+1]
		ctl.runnableVal[i] = ctl.runnableVal[i+1]
		ctl.runnableValSet[i] = ctl.runnableValSet[i+1]
		ctl.runnableSize[i] = ctl.runnableSize[i+1]
	}
	ctl.nrun--
	ctl.runnable[ctl.nrun] = 0
	return gp
}

// weaveStart enrolls the bubble's main goroutine (created parked) and grants it
// the run token. Runs on the system stack from weaveRunBubble.
func weaveStart(bubble *synctestBubble, main *g) {
	ctl := bubble.weaveCtl
	lock(&bubble.mu)
	ctl.live++
	weaveAssignWid(ctl, main)
	weavePush(ctl, main, weaveOpNone, 0, 0, 0, 0, false)
	next := weaveTake(ctl)
	unlock(&bubble.mu)
	weaveGrant(next)
}

// weaveAssignWid gives gp its stable participant id and records its creation
// site (the go-statement PC) so the report can name goroutines by where they
// were spawned. Caller holds bubble.mu.
func weaveAssignWid(ctl *weaveControl, gp *g) {
	gp.weaveWid = ctl.nextWid
	ctl.nextWid++
	if ctl.nextWid > 64 {
		ctl.overflow = true // too many participants for DPOR's 64-bit enabled mask
	}
	if ctl.spawnPC != nil && int(gp.weaveWid) < len(ctl.spawnPC) {
		ctl.spawnPC[gp.weaveWid] = uint64(gp.gopc)
	}
}

// weaveRegisterChild enrolls a newly created (parked) participant. The creator
// keeps the run token and continues; the child waits its turn.
func weaveRegisterChild(bubble *synctestBubble, newg *g) {
	lock(&bubble.mu)
	ctl := bubble.weaveCtl
	ctl.live++
	weaveAssignWid(ctl, newg)
	weavePush(ctl, newg, weaveOpNone, 0, 0, 0, 0, false)
	unlock(&bubble.mu)
}

// weaveGoWrapper is the entry point of every controlled participant. It runs the
// participant's real function with a deferred recover, so a panic in a spawned
// goroutine (e.g. a nil-pointer dereference from a concurrency bug) is captured
// as a reported failure with its interleaving, instead of crashing the process.
func weaveGoWrapper() {
	gp := getg()
	fn := gp.weaveFn
	gp.weaveFn = nil
	defer weaveRecoverChild()
	f := *(*func())(unsafe.Pointer(&fn))
	f()
}

// weaveRecoverChild records a participant's panic on the bubble. Runs as the
// deferred function of weaveGoWrapper.
func weaveRecoverChild() {
	if r := recover(); r != nil {
		b := getg().bubble
		lock(&b.mu)
		if !b.weaveCtl.panicked {
			b.weaveCtl.panicked = true
			b.weaveCtl.panicValue = r
		}
		unlock(&b.mu)
	}
}

// weaveNewParticipant creates a parked participant that starts in weaveGoWrapper
// and runs fn. Runs on the system stack.
func weaveNewParticipant(callergp *g, pc uintptr, fn *funcval) *g {
	wf := weaveGoWrapper
	wfv := *(**funcval)(unsafe.Pointer(&wf))
	newg := newproc1(wfv, callergp, pc, true, waitReasonWeaveScheduled)
	newg.weaveFn = fn
	return newg
}

// weaveEnqueue captures a participant woken by a synchronization operation into
// the runnable set. It stays parked (resuming only when granted), preserving
// serialization. Called from ready; must not use write barriers.
func weaveEnqueue(gp *g) {
	b := gp.bubble
	lock(&b.mu)
	// Woken from a block; its next operation is not yet known.
	weavePush(b.weaveCtl, gp, weaveOpNone, 0, 0, 0, 0, false)
	unlock(&b.mu)
}

// weaveOnBlock is called from park_m when a participant blocks on a real
// synchronization operation. It passes the token to the chosen runnable
// participant; if there is none, the bubble is deadlocked.
func weaveOnBlock(bubble *synctestBubble) {
	ctl := bubble.weaveCtl
	lock(&bubble.mu)
	next := weaveTake(ctl)
	if next != nil {
		unlock(&bubble.mu)
		weaveGrant(next)
		return
	}
	// No participant is runnable. If a timer is pending, the bubble is not
	// deadlocked: the fake clock can advance and fire it, waking a participant.
	// The root drives that (weaveRootWait). Otherwise it is a real deadlock.
	if bubble.timers.wakeTime() > 0 {
		ctl.advanceClock = true
	} else {
		ctl.deadlock = true
	}
	wake := ctl.rootParked
	ctl.rootParked = false
	unlock(&bubble.mu)
	if wake {
		goready(bubble.root, 0)
	}
}

// weaveOnGoexit is called when a participant finishes. It hands the token to the
// chosen runnable participant, or wakes the root when the bubble is finished
// (all exited) or deadlocked (some remain but none runnable).
func weaveOnGoexit(bubble *synctestBubble) {
	ctl := bubble.weaveCtl
	lock(&bubble.mu)
	ctl.live--
	// A participant parked in weave.Wait must re-check now that one fewer
	// participant is live: re-enqueue it so the controller can resume it.
	if ctl.waiter != nil {
		weavePush(ctl, ctl.waiter, weaveOpNone, 0, 0, 0, 0, false)
		ctl.waiter = nil
	}
	next := weaveTake(ctl)
	if next != nil {
		unlock(&bubble.mu)
		weaveGrant(next)
		return
	}
	if ctl.live == 0 {
		ctl.done = true
	} else if bubble.timers.wakeTime() > 0 {
		// Participants remain but none is runnable; a pending timer can still make
		// progress once the fake clock advances (driven by the root).
		ctl.advanceClock = true
	} else {
		ctl.deadlock = true
	}
	wake := ctl.rootParked
	ctl.rootParked = false
	unlock(&bubble.mu)
	if wake {
		goready(bubble.root, 0)
	}
}

// weaveSchedPoint is an explicit scheduling point. Returns immediately unless
// the caller is a participant in a controlled bubble.
//
// The //go:linkname makes it accessible to internal/sync for mutex recording.
//
//go:linkname weaveSchedPoint
//go:nosplit
func weaveSchedPoint(op weaveOp, id unsafe.Pointer) {
	if !weaveActive() {
		return
	}
	weaveSchedPointSlow(op, id, 0, 0, 0, false)
}

func weaveSchedPointSlow(op weaveOp, id unsafe.Pointer, size, pc uintptr, val uint64, valSet bool) {
	gp := getg()
	bubble := gp.bubble
	ctl := bubble.weaveCtl
	lock(&bubble.mu)
	// gp holds the token; offer to yield by joining the runnable set with its
	// pending operation (and, for a memory op, its width/PC/value) and taking the
	// chosen one. If gp is chosen, it keeps running.
	weavePush(ctl, gp, op, uintptr(id), size, pc, val, valSet)
	next := weaveTake(ctl)
	if next == gp {
		unlock(&bubble.mu)
		return
	}
	unlock(&bubble.mu)
	// Park gp; grant next in the unlockf, after gp is off its M.
	gopark(weaveHandoff, unsafe.Pointer(next), waitReasonWeaveScheduled, traceBlockSynctest, 0)
}

// weaveread/weavewrite/weavereadrange/weavewriterange are the memory-access
// instrumentation hooks the compiler inserts (under -weave) before loads and
// stores, exactly where the race detector inserts raceread/racewrite. Each is a
// scheduling point when the current goroutine is a controlled participant, so
// ordinary reads and writes of shared memory (plain variables, struct fields,
// slice/array elements, ...) become interleaving points with no source changes.
// They are no-ops for non-participants (including the driver goroutine), so
// there is no recursion through the runtime (which is never instrumented).

func weaveread(addr, size uintptr) {
	if weaveActive() {
		weaveSchedPointSlow(weaveOpRead, unsafe.Pointer(addr), size, sys.GetCallerPC(), 0, false)
		// After the scheduling point the participant holds the token and is about to
		// perform the real load, with no other participant running in between, so the
		// value here is exactly what it observes (fresh even if it parked at this
		// read while another participant wrote). Sample it now, in participant
		// context: a fault on an unmapped address is a recoverable panic (subject to
		// the model's runtime.SetPanicOnFault) rather than a controller-context
		// process-fatal. Backfill it into the transition just recorded.
		val, valSet := weaveLoadVal(addr, size)
		weaveBackfillReadVal(val, valSet)
	}
}

// weaveBackfillReadVal records the value observed by the just-executed read into
// its transition (the most recent step for this participant, recorded when it was
// granted the token). No-op outside a controlled bubble or if the last step is
// not this participant's read.
func weaveBackfillReadVal(val uint64, valSet bool) {
	gp := getg()
	b := gp.bubble
	if b == nil || !b.controlled {
		return
	}
	ctl := b.weaveCtl
	lock(&b.mu)
	if ctl.traceVal != nil {
		s := ctl.step - 1
		if s >= 0 && s < len(ctl.traceVal) && ctl.traceWid[s] == gp.weaveWid && ctl.traceOp[s] == int32(weaveOpRead) {
			ctl.traceVal[s] = val
			if valSet {
				ctl.traceValSet[s] = 1
			} else {
				ctl.traceValSet[s] = 0
			}
		}
	}
	unlock(&b.mu)
}

func weavewrite(addr, size uintptr) {
	if weaveActive() {
		// Used for scalar stores whose value the compiler could not supply (float,
		// pointer, ...); record the write as a scheduling point but no value.
		weaveSchedPointSlow(weaveOpWrite, unsafe.Pointer(addr), size, sys.GetCallerPC(), 0, false)
	}
}

// weavewriteval is the store hook for integer/bool scalars, where the compiler
// passes the value being written so the trace can show the new value.
func weavewriteval(addr, size uintptr, val uint64) {
	if weaveActive() {
		weaveSchedPointSlow(weaveOpWrite, unsafe.Pointer(addr), size, sys.GetCallerPC(), val, true)
	}
}

func weavereadrange(addr, size uintptr) {
	if weaveActive() {
		weaveSchedPointSlow(weaveOpRead, unsafe.Pointer(addr), size, sys.GetCallerPC(), 0, false)
	}
}

func weavewriterange(addr, size uintptr) {
	if weaveActive() {
		weaveSchedPointSlow(weaveOpWrite, unsafe.Pointer(addr), size, sys.GetCallerPC(), 0, false)
	}
}

// weaveLoadVal reads a scalar of the given size at addr into a uint64 for display
// in a failing interleaving. It only handles the machine-word sizes; other sizes
// (composites) report no value.
func weaveLoadVal(addr, size uintptr) (uint64, bool) {
	switch size {
	case 1:
		return uint64(*(*uint8)(unsafe.Pointer(addr))), true
	case 2:
		return uint64(*(*uint16)(unsafe.Pointer(addr))), true
	case 4:
		return uint64(*(*uint32)(unsafe.Pointer(addr))), true
	case 8:
		return *(*uint64)(unsafe.Pointer(addr)), true
	}
	return 0, false
}

// weaveHandoff is the gopark unlockf for an explicit yield: it grants the token
// to next after the yielding goroutine has been switched off its M.
func weaveHandoff(gp *g, next unsafe.Pointer) bool {
	weaveGrant((*g)(next))
	return true
}

// weaveGrant makes a participant runnable and gives it the run token. It mirrors
// ready but bypasses ready's weave interception (which would re-enqueue the
// participant instead of running it).
func weaveGrant(gp *g) {
	mp := acquirem()
	trace := traceAcquire()
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.ok() {
		trace.GoUnpark(gp, 0)
		traceRelease(trace)
	}
	runqput(mp.p.ptr(), gp, true)
	wakep()
	releasem(mp)
}

// weaveRootWait drives the controlled bubble from the root goroutine: it parks
// until the controller reports the bubble finished or deadlocked, and — when the
// bubble is quiescent but a timer is pending — advances the fake clock and fires
// due timers so time.Sleep/time.After/timers make progress under weave. This is
// the analogue of synctest's fake-time quiescence loop, integrated with the
// run-token handoff.
func weaveRootWait(bubble *synctestBubble) {
	root := getg()
	ctl := bubble.weaveCtl
	for {
		lock(&bubble.mu)
		if ctl.done || ctl.deadlock {
			unlock(&bubble.mu)
			return
		}
		if ctl.advanceClock {
			// All participants are durably blocked but a timer is pending. Advance
			// the fake clock to the next deadline and fire the due timers; their
			// callbacks wake participants via ready -> weaveEnqueue. Then grant the
			// token to one of them and resume driving.
			ctl.advanceClock = false
			if next := bubble.timers.wakeTime(); next > bubble.now {
				bubble.now = next
			}
			unlock(&bubble.mu)
			// Clear m.curg while running timers so timer goroutines inherit their
			// race context from g0, as synctest does.
			systemstack(func() {
				curg := root.m.curg
				root.m.curg = nil
				bubble.timers.check(bubble.now, bubble)
				root.m.curg = curg
			})
			lock(&bubble.mu)
			next := weaveTake(ctl)
			if next != nil {
				unlock(&bubble.mu)
				weaveGrant(next)
				continue
			}
			// The timers fired but woke no participant. Decide the next state.
			switch {
			case ctl.live == 0:
				ctl.done = true
			case bubble.timers.wakeTime() > 0:
				ctl.advanceClock = true // more timers remain; advance again
			default:
				ctl.deadlock = true
			}
			unlock(&bubble.mu)
			continue
		}
		unlock(&bubble.mu)
		gopark(weaveRootPark, unsafe.Pointer(bubble), waitReasonSynctestRun, traceBlockSynctest, 0)
	}
}

// weaveRootPark is the root's gopark unlockf. It runs after the root is already
// _Gwaiting, so setting rootParked here (rather than before gopark) closes the
// wake-before-park race: a participant only calls goready(root) when it observes
// rootParked, which is true only once the root is genuinely parked. If the
// bubble already finished/deadlocked, it declines to park and the root resumes.
func weaveRootPark(gp *g, arg unsafe.Pointer) bool {
	b := (*synctestBubble)(arg)
	lock(&b.mu)
	// Decline to park (root loops and re-checks) if the bubble finished,
	// deadlocked, or needs the clock advanced — the last closes the
	// wake-before-park race for a timer signalled just before the root parks.
	if b.weaveCtl.done || b.weaveCtl.deadlock || b.weaveCtl.advanceClock {
		unlock(&b.mu)
		return false
	}
	b.weaveCtl.rootParked = true
	unlock(&b.mu)
	return true
}

// weaveRunBubble runs one schedule of f in a fresh controlled bubble driven by
// ctl. It returns after the bubble finishes or deadlocks; the outcome is in ctl.
func weaveRunBubble(f func(), ctl *weaveControl) {
	gp := getg()
	if gp.bubble != nil {
		panic("weave.Run called from within a bubble")
	}
	bubble := &synctestBubble{
		id:         bubbleGen.Add(1),
		root:       gp,
		controlled: true,
		weaveCtl:   ctl,
	}
	lockInit(&bubble.mu, lockRankSynctest)
	lockInit(&bubble.timers.mu, lockRankTimers)
	gp.bubble = bubble
	atomic.Xadd(&weaveGloballyActive, 1)
	defer func() {
		atomic.Xadd(&weaveGloballyActive, -1)
		gp.bubble = nil
	}()

	pc := sys.GetCallerPC()
	systemstack(func() {
		fv := *(**funcval)(unsafe.Pointer(&f))
		main := weaveNewParticipant(gp, pc, fv)
		bubble.main = main
		weaveStart(bubble, main)
	})
	weaveRootWait(bubble)
}

// weaveRunSchedule runs f once in a fresh controlled bubble, forcing the choices
// in plan and recording the choices actually taken and the number of options at
// each scheduling point into choices/branches (all may be nil for a plain FIFO
// run). It returns how many scheduling points were reached and an outcome:
// 0 = finished normally, 1 = deadlock, 2 = ran out of recording space.
//
// The exploration driver (internal/weave.Explore) lives in Go so it can wrap f
// with a deferred recover to capture model panics; this is the runtime primitive
// it calls once per schedule.
//
//go:linkname weaveRunSchedule internal/weave.runSchedule
func weaveRunSchedule(f func(), plan, traceWid, traceOp, traceValSet, selPlan, selTrace, selBranch, selStepIdx []int32, traceAddr, traceSize []int64, traceEnabled, tracePC, spawnPC, traceVal []uint64, sticky bool) (steps, nsel, outcome int, failure any) {
	ctl := new(weaveControl)
	ctl.stickyDefault = sticky
	ctl.lastWid = -1
	ctl.plan = plan
	ctl.traceWid = traceWid
	ctl.traceOp = traceOp
	ctl.traceAddr = traceAddr
	ctl.traceSize = traceSize
	ctl.traceEnabled = traceEnabled
	ctl.tracePC = tracePC
	ctl.spawnPC = spawnPC
	ctl.traceVal = traceVal
	ctl.traceValSet = traceValSet
	ctl.selPlan = selPlan
	ctl.selTrace = selTrace
	ctl.selBranch = selBranch
	ctl.selStepIdx = selStepIdx
	weaveRunBubble(f, ctl)
	failure = ctl.panicValue
	switch {
	case ctl.overflow:
		// Capacity overflow takes priority: a run that overflowed may also look
		// deadlocked (a dropped participant inflates the live count), but the
		// honest outcome is "capacity limit / truncated", not a real deadlock.
		outcome = 2
	case ctl.deadlock:
		outcome = 1
	}
	return ctl.step, ctl.selStep, outcome, failure
}

// weaveSelectChoose picks which of the nready ready cases of a controlled select
// fires, recording the choice as a select event so the driver can enumerate the
// alternatives. It does not hand off the run token: the choice is internal to the
// running participant (made while it holds the token, so the select commits
// atomically). Returns the chosen index in [0, nready). Called from select.go.
func weaveSelectChoose(nready int32) int32 {
	gp := getg()
	b := gp.bubble
	if b == nil || !b.controlled {
		return 0
	}
	ctl := b.weaveCtl
	choice := int32(0)
	if ctl.selStep < len(ctl.selPlan) {
		if c := ctl.selPlan[ctl.selStep]; c >= 0 && c < nready {
			choice = c
		}
	}
	if ctl.selTrace != nil {
		if ctl.selStep < len(ctl.selTrace) {
			ctl.selTrace[ctl.selStep] = choice
			ctl.selBranch[ctl.selStep] = nready
			ctl.selStepIdx[ctl.selStep] = int32(ctl.step - 1)
		} else {
			ctl.overflow = true
		}
	}
	ctl.selStep++
	return choice
}

// weaveYield is an explicit scheduling point: it offers to hand the run token to
// another participant.
//
//go:linkname weaveYield internal/weave.Yield
func weaveYield() {
	weaveSchedPoint(weaveOpNone, nil)
}

// weaveWait blocks the calling participant until every other participant in the
// bubble has finished. It hands off the run token and parks; each time another
// participant exits (weaveOnGoexit) the waiter is re-enqueued to re-check. If no
// other participant can run while some remain, that is a deadlock.
//
//go:linkname weaveWait internal/weave.Wait
func weaveWait() {
	gp := getg()
	b := gp.bubble
	if b == nil || !b.controlled {
		return
	}
	ctl := b.weaveCtl
	for {
		lock(&b.mu)
		if ctl.live <= 1 {
			unlock(&b.mu)
			return
		}
		if ctl.waiter != nil && ctl.waiter != gp {
			unlock(&b.mu)
			panic("weave: concurrent weave.Wait")
		}
		ctl.waiter = gp
		next := weaveTake(ctl)
		if next == nil {
			// Others remain but none can run: they can never exit → deadlock.
			ctl.waiter = nil
			ctl.deadlock = true
			wake := ctl.rootParked
			ctl.rootParked = false
			unlock(&b.mu)
			if wake {
				goready(b.root, 0)
			}
			gopark(weaveDeadPark, nil, waitReasonWeaveScheduled, traceBlockSynctest, 0)
			return
		}
		unlock(&b.mu)
		// Park; grant next in the unlockf. Resumed when re-enqueued on a goexit.
		gopark(weaveHandoff, unsafe.Pointer(next), waitReasonWeaveScheduled, traceBlockSynctest, 0)
	}
}

// weaveDeadPark parks the caller with no wakeup; used after a deadlock is
// declared, until the run is torn down.
func weaveDeadPark(gp *g, _ unsafe.Pointer) bool { return true }

// weaveOp identifies the kind of scheduling point (reserved for the exploration
// engine's happens-before analysis; not yet used by the exhaustive policy).
type weaveOp uint8

const (
	weaveOpNone weaveOp = iota
	weaveOpRead
	weaveOpWrite
	weaveOpLock
	weaveOpUnlock
	weaveOpChanSend
	weaveOpChanRecv
	weaveOpChanClose
	weaveOpSelect
	weaveOpWaitGroupWait
	weaveOpGoStart
	weaveOpGoExit
	weaveOpPreempt
	weaveOpWaitGroupAdd
	weaveOpCondWait
	weaveOpCondSignal
	weaveOpCondBroadcast
	weaveOpOnce
	// Non-blocking channel ops (from a select with a default). They are
	// scheduling points so their success/failure reflects the interleaving, but
	// they establish no happens-before (channelHB ignores them): a poll that
	// receives nothing must not be matched against a sender.
	weaveOpChanSendNB
	weaveOpChanRecvNB
)
