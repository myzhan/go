// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

// weave operation codes, kept in sync with runtime weaveOp (see
// runtime/weave.go); internal/weave/optab_test.go asserts they agree.
//
// Every hook site in this package is written `if weaveEnabled { weaveSchedPoint(…) }`
// with weaveEnabled a build-tagged constant (weave_on.go / weave_off.go), so in an
// ordinary build the hook is dead code the compiler drops before the inliner even
// prices the function. That matters because RWMutex.RLock/RUnlock and Once.Do sit
// at (or within a point or two of) the inliner's budget upstream: a *call* to even
// an empty function costs 57 there, and an unconditional hook cost them their
// inlining in every program built with this toolchain.
//
// The trade-off is that these primitives are scheduling points only under -weave.
// internal/sync.Mutex is the deliberate exception — its hook is always compiled and
// gated at run time, so internal/weave can explore mutex interleavings with a plain
// `go test` (see internal/sync/weave.go and .claude/weave/design.md D20).
const (
	weaveOpLock          = 3
	weaveOpUnlock        = 4
	weaveOpWaitGroupWait = 9
	weaveOpWaitGroupAdd  = 13
	weaveOpCondWait      = 14
	weaveOpCondSignal    = 15
	weaveOpCondBroadcast = 16
	weaveOpOnce          = 17
	weaveOpRLock         = 23
	weaveOpRUnlock       = 24
)
