// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

package sync

import "unsafe" // also for linkname

// weaveGloballyActive is nonzero only while a weave controlled bubble exists, so
// the hooks cost a single global load in the common case. runtime_weaveSchedPointSkip
// records the operation as a weave transition; it is a no-op outside a controlled
// bubble.
//
// weaveEnabled gates the hook call sites at compile time, so ordinary builds pay
// nothing at all (see weave_off.go).
const weaveEnabled = true

//go:linkname weaveGloballyActive runtime.weaveGloballyActive
var weaveGloballyActive uint32

//go:linkname runtime_weaveSchedPointSkip runtime.weaveSchedPointSkip
func runtime_weaveSchedPointSkip(op uint8, addr unsafe.Pointer, skip int)

// weaveSchedPoint records a synchronization operation on obj as a weave
// scheduling point.
//
// It is deliberately noinline: -weave instruments memory accesses in the
// command-line packages only, but an inlined body is instrumented in whatever
// package it lands in. Keeping this body out of the caller keeps the
// weaveGloballyActive load from itself becoming a scheduling point (which would
// add a spurious transition to every sync operation and inflate the state space).
//
// The skip count reaches past this helper and the primitive that called it (Lock,
// Do, Wait, ...) to the line the user wrote, so a failing trace points at their code
// rather than at sync's.
//
//go:noinline
func weaveSchedPoint(op uint8, obj unsafe.Pointer) {
	if weaveGloballyActive != 0 {
		runtime_weaveSchedPointSkip(op, obj, 4)
	}
}
