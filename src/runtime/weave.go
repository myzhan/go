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
	runnable     [weaveMaxRunnable]guintptr
	runnableWid  [weaveMaxRunnable]int32
	runnableOp   [weaveMaxRunnable]uint8
	runnableAddr [weaveMaxRunnable]uintptr
	nrun         int // number of valid entries

	nextWid    int32  // next participant id to assign
	live       int    // participants enrolled and not yet exited
	done       bool   // all participants have exited
	deadlock   bool   // some remain but none can run
	rootParked bool   // root goroutine is parked in weaveRootWait
	waiter     *g     // participant parked in weave.Wait, or nil
	randState  uint64 // deterministic RNG state for this run
	panicked   bool   // a participant panicked
	panicValue any    // the recovered panic value

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
	step         int
	overflow     bool // ran out of recording space or too many participants for DPOR

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
// operation. Caller holds bubble.mu.
func weavePush(ctl *weaveControl, gp *g, op weaveOp, addr uintptr) {
	if ctl.nrun >= weaveMaxRunnable {
		throw("weave: too many runnable goroutines")
	}
	i := ctl.nrun
	ctl.runnable[i].set(gp)
	ctl.runnableWid[i] = gp.weaveWid
	ctl.runnableOp[i] = uint8(op)
	ctl.runnableAddr[i] = addr
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
		idx = weaveLowestWid(ctl)
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
			// A source PC is only meaningful for memory read/write ops; other
			// transitions (run/exit/chan/mutex) reuse the g and would show a
			// stale PC.
			if o := ctl.runnableOp[idx]; o == uint8(weaveOpRead) || o == uint8(weaveOpWrite) {
				gp := ctl.runnable[idx].ptr()
				ctl.tracePC[ctl.step] = uint64(gp.weavePC)
				if ctl.traceVal != nil {
					ctl.traceVal[ctl.step] = gp.weaveVal
					if gp.weaveValSet {
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
	ctl.step++
	return idx
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
	weavePush(ctl, main, weaveOpNone, 0)
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
	weavePush(ctl, newg, weaveOpNone, 0)
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
	weavePush(b.weaveCtl, gp, weaveOpNone, 0)
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
	ctl.deadlock = true
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
		weavePush(ctl, ctl.waiter, weaveOpNone, 0)
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
	weaveSchedPointSlow(op, id, 0)
}

func weaveSchedPointSlow(op weaveOp, id unsafe.Pointer, pc uintptr) {
	gp := getg()
	gp.weavePC = pc
	bubble := gp.bubble
	ctl := bubble.weaveCtl
	lock(&bubble.mu)
	// gp holds the token; offer to yield by joining the runnable set with its
	// pending operation and taking the chosen one. If gp is chosen, it keeps
	// running.
	weavePush(ctl, gp, op, uintptr(id))
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
		gp := getg()
		gp.weaveVal, gp.weaveValSet = weaveLoadVal(addr, size)
		weaveSchedPointSlow(weaveOpRead, unsafe.Pointer(addr), sys.GetCallerPC())
	}
}

func weavewrite(addr, size uintptr) {
	if weaveActive() {
		// Used for scalar stores whose value the compiler could not supply (float,
		// pointer, ...); record the write as a scheduling point but no value.
		getg().weaveValSet = false
		weaveSchedPointSlow(weaveOpWrite, unsafe.Pointer(addr), sys.GetCallerPC())
	}
}

// weavewriteval is the store hook for integer/bool scalars, where the compiler
// passes the value being written so the trace can show the new value.
func weavewriteval(addr, size uintptr, val uint64) {
	if weaveActive() {
		gp := getg()
		gp.weaveVal, gp.weaveValSet = val, true
		weaveSchedPointSlow(weaveOpWrite, unsafe.Pointer(addr), sys.GetCallerPC())
	}
}

func weavereadrange(addr, size uintptr) {
	if weaveActive() {
		getg().weaveValSet = false // composite: no meaningful scalar value
		weaveSchedPointSlow(weaveOpRead, unsafe.Pointer(addr), sys.GetCallerPC())
	}
}

func weavewriterange(addr, size uintptr) {
	if weaveActive() {
		getg().weaveValSet = false // composite: no meaningful scalar value
		weaveSchedPointSlow(weaveOpWrite, unsafe.Pointer(addr), sys.GetCallerPC())
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

// weaveRootWait parks the root goroutine until the controller reports the bubble
// finished or deadlocked.
func weaveRootWait(bubble *synctestBubble) {
	for {
		lock(&bubble.mu)
		fin := bubble.weaveCtl.done || bubble.weaveCtl.deadlock
		unlock(&bubble.mu)
		if fin {
			return
		}
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
	if b.weaveCtl.done || b.weaveCtl.deadlock {
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
func weaveRunSchedule(f func(), plan, traceWid, traceOp, traceValSet, selPlan, selTrace, selBranch, selStepIdx []int32, traceAddr []int64, traceEnabled, tracePC, spawnPC, traceVal []uint64) (steps, nsel, outcome int, failure any) {
	ctl := new(weaveControl)
	ctl.plan = plan
	ctl.traceWid = traceWid
	ctl.traceOp = traceOp
	ctl.traceAddr = traceAddr
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
	case ctl.deadlock:
		outcome = 1
	case ctl.overflow:
		outcome = 2
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
)
