// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package weave provides the runtime bridge for weave's controlled scheduler.
// The low-level primitives (runSchedule, Yield) live in the runtime and are
// attached here via //go:linkname; the exploration driver (Explore) lives here
// in Go so it can wrap the model with a deferred recover to capture panics.
//
// weave is always compiled in and runs under a plain `go test`; it has no effect
// unless a controlled bubble is created via Run/Explore.
//
// This is not a public API; testing/synctest drives it under -weave.
package weave

import _ "unsafe" // for linkname

// Yield is an explicit scheduling point: it offers to hand the run token to
// another participant. Used to create interleaving opportunities where the model
// has no other synchronization operation (until compiler instrumentation makes
// memory accesses scheduling points).
//
//go:linkname Yield
func Yield()

// Wait blocks the calling goroutine until every other goroutine in the bubble
// has finished. It is the weave analogue of sync.WaitGroup.Wait for joining
// spawned work before asserting a post-condition.
//
//go:linkname Wait
func Wait()

// runSchedule runs f once in a fresh controlled bubble, forcing at scheduling
// point i the participant whose stable id is plan[i] (beyond len(plan), the
// lowest runnable id). It records, per step, the participant that ran (traceWid),
// its operation and address (traceOp/traceAddr), and a bitmask of the runnable
// participants (traceEnabled). It returns the number of scheduling points reached
// and an outcome: 0 = finished, 1 = deadlock, 2 = out of space / too many procs.
//
// The final sticky argument selects a preemption-free default policy (continue
// the previous participant while it is runnable) so a context-bounded search
// never executes a schedule exceeding its preemption bound; only the bounded
// exploration path sets it.
//
//go:linkname runSchedule
func runSchedule(f func(), plan, traceWid, traceOp, traceValSet, selPlan, selTrace, selBranch, selStepIdx []int32, traceAddr, traceSize []int64, traceEnabled, tracePC, spawnPC, traceVal []uint64, sticky bool) (steps, nsel, outcome int, failure any, unfiredTimer bool)
