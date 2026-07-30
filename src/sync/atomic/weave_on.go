// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

package atomic

import "unsafe"

// Under -weave, every typed atomic operation is a scheduling point, so weave can
// interleave lock-free code — e.g. a non-atomic Load()+Store() read-modify-write.
// weaveGloballyActive is nonzero while a weave controlled bubble exists; when it
// is zero the hook is skipped with a single load. runtime_weaveSchedPoint records
// the operation as a weave transition (yielding to the controlled scheduler) and
// is a no-op outside a controlled bubble. This mirrors internal/sync's mutex hook.
//
// weaveEnabled gates the hook call sites in type.go at compile time, so ordinary
// builds pay nothing at all (see weave_off.go).
const weaveEnabled = true

//go:linkname weaveGloballyActive runtime.weaveGloballyActive
var weaveGloballyActive uint32

//go:linkname runtime_weaveSchedPointSkip runtime.weaveSchedPointSkip
func runtime_weaveSchedPointSkip(op uint8, addr unsafe.Pointer, skip int)

// weaveAtomic is deliberately noinline: -weave instruments memory accesses in the
// command-line packages only, but an inlined body is instrumented in whatever
// package it lands in — and the typed atomic methods DO inline into their callers.
// Keeping this body out of the caller keeps the weaveGloballyActive load from
// itself becoming a scheduling point, which otherwise bracketed every atomic
// operation with two spurious "read" transitions and roughly doubled the state
// space of atomic-heavy models.
//
//go:noinline
//go:nosplit
func weaveAtomic(op uint8, addr unsafe.Pointer) {
	if weaveGloballyActive != 0 {
		// Reach past this helper and the typed method to the caller's line.
		runtime_weaveSchedPointSkip(op, addr, 4)
	}
}
