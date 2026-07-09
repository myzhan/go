// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import "runtime"

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
)

// Step is one transition in a recorded interleaving.
type Step struct {
	Wid  int    // participant id
	Op   string // operation kind
	Addr uint64 // object/variable address, or 0
	File string // source file of the operation, or "" if unknown
	Line int    // source line, or 0
}

// Result summarizes an exploration.
type Result struct {
	Runs     int    // schedules explored
	Deadlock bool   // a schedule deadlocked
	Failed   bool   // a schedule panicked (Value holds the recovered value)
	Value    any    // recovered panic value from the failing schedule
	Seed     string // reproducible seed for the failing/deadlocking schedule
	Trace    []Step // the failing/deadlocking interleaving
}

// Run executes f once in a controlled bubble (a single default schedule).
// It panics if the run deadlocks.
func Run(f func()) {
	_, outcome := runSchedule(f, nil, nil, nil, nil, nil, nil)
	if outcome == 1 {
		panic("weave: deadlock: all goroutines in bubble are blocked")
	}
}

const traceCap = 1 << 16

type explorer struct {
	traceWid     []int32
	traceOp      []int32
	traceAddr    []int64
	traceEnabled []uint64
	tracePC      []uint64
	res          Result
	wrapped      func()
}

func newExplorer(f func()) *explorer {
	e := &explorer{
		traceWid:     make([]int32, traceCap),
		traceOp:      make([]int32, traceCap),
		traceAddr:    make([]int64, traceCap),
		traceEnabled: make([]uint64, traceCap),
		tracePC:      make([]uint64, traceCap),
	}
	e.wrapped = func() {
		defer func() {
			if r := recover(); r != nil && !e.res.Failed {
				e.res.Failed = true
				e.res.Value = r
			}
		}()
		f()
	}
	return e
}

// conflict reports whether two transitions by different participants are
// dependent: they touch the same object and their order can change the outcome.
// Unknown ops (opNone, from a freshly spawned or just-woken participant) and
// distinct addresses are independent. Read/read on the same address is
// independent; any write, or any synchronization-object operation, conflicts.
func conflict(opi uint8, ai int64, opj uint8, aj int64) bool {
	if ai == 0 || aj == 0 || ai != aj {
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

func isSyncOp(op uint8) bool {
	switch op {
	case opLock, opUnlock, opChanSend, opChanRecv, opChanClose, opSelect, opWaitGroupWait:
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
func Explore(f func()) Result {
	e := newExplorer(f)

	type frame struct {
		backtrack map[int32]bool
		done      map[int32]bool
	}
	var stack []*frame
	var plan []int32

	for {
		steps, outcome := runSchedule(e.wrapped, plan, e.traceWid, e.traceOp, e.traceAddr, e.traceEnabled, e.tracePC)
		e.res.Runs++

		wid := e.traceWid[:steps]
		op := e.traceOp[:steps]
		addr := e.traceAddr[:steps]
		en := e.traceEnabled[:steps]

		if e.res.Failed {
			e.res.Seed = encodeSeed(wid)
			e.res.Trace = buildTrace(wid, op, addr, e.tracePC[:steps])
			return e.res
		}
		if outcome == 1 {
			e.res.Deadlock = true
			e.res.Seed = encodeSeed(wid)
			e.res.Trace = buildTrace(wid, op, addr, e.tracePC[:steps])
			return e.res
		}
		if outcome == 2 {
			return e.res // out of space / too many participants
		}

		for len(stack) < steps {
			stack = append(stack, &frame{backtrack: map[int32]bool{}, done: map[int32]bool{}})
		}
		for i := 0; i < steps; i++ {
			stack[i].backtrack[wid[i]] = true
			stack[i].done[wid[i]] = true
		}

		// Backward analysis: for each transition j, find the nearest earlier
		// concurrent transition i it conflicts with, and schedule the reversal
		// (run j's participant at state i) in a future run.
		for j := 0; j < steps; j++ {
			for i := j - 1; i >= 0; i-- {
				if wid[i] == wid[j] {
					continue // program-order predecessor of j (happens-before j)
				}
				if conflict(uint8(op[i]), addr[i], uint8(op[j]), addr[j]) {
					wj := wid[j]
					if en[i]&(1<<uint(wj)) != 0 {
						if !stack[i].done[wj] {
							stack[i].backtrack[wj] = true
						}
					} else {
						// j's participant was not runnable at state i; explore all
						// runnable participants there to reach the reversal.
						for w := int32(0); w < 64; w++ {
							if en[i]&(1<<uint(w)) != 0 && !stack[i].done[w] {
								stack[i].backtrack[w] = true
							}
						}
					}
					break // nearest conflicting transition only
				}
			}
		}

		// Pick the deepest frame with an unexplored backtrack entry.
		d, pick := -1, int32(-1)
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
		}
		if d < 0 {
			return e.res
		}
		stack[d].done[pick] = true
		plan = append(plan[:0], wid[:d]...)
		plan = append(plan, pick)
		stack = stack[:d+1] // discard the now-stale subtree below d
	}
}

// Replay deterministically re-runs the single interleaving encoded by seed (as
// produced in Result.Seed), so a discovered failure can be reproduced.
func Replay(seed string, f func()) Result {
	e := newExplorer(f)
	plan := decodeSeed(seed)
	steps, outcome := runSchedule(e.wrapped, plan, e.traceWid, e.traceOp, e.traceAddr, e.traceEnabled, e.tracePC)
	e.res.Runs = 1
	e.res.Seed = seed
	e.res.Trace = buildTrace(e.traceWid[:steps], e.traceOp[:steps], e.traceAddr[:steps], e.tracePC[:steps])
	if outcome == 1 {
		e.res.Deadlock = true
	}
	return e.res
}

func buildTrace(wid, op []int32, addr []int64, pc []uint64) []Step {
	t := make([]Step, len(wid))
	for i := range wid {
		s := Step{Wid: int(wid[i]), Op: opName(uint8(op[i])), Addr: uint64(addr[i])}
		if pc[i] != 0 {
			fr, _ := runtime.CallersFrames([]uintptr{uintptr(pc[i])}).Next()
			s.File, s.Line = fr.File, fr.Line
		}
		t[i] = s
	}
	return t
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
	case opSelect:
		return "select"
	case opWaitGroupWait:
		return "wg wait"
	}
	return "run"
}

func encodeSeed(wid []int32) string {
	if len(wid) == 0 {
		return ""
	}
	b := make([]byte, 0, len(wid)*3)
	for i, w := range wid {
		if i > 0 {
			b = append(b, '.')
		}
		b = appendInt(b, int(w))
	}
	return string(b)
}

func decodeSeed(seed string) []int32 {
	if seed == "" {
		return nil
	}
	var out []int32
	n, has := 0, false
	for i := 0; i <= len(seed); i++ {
		if i < len(seed) && seed[i] >= '0' && seed[i] <= '9' {
			n = n*10 + int(seed[i]-'0')
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
		steps, outcome := runSchedule(e.wrapped, plan, e.traceWid, e.traceOp, e.traceAddr, e.traceEnabled, e.tracePC)
		e.res.Runs++
		if e.res.Failed {
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
