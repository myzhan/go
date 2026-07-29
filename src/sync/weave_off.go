// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !weave

package sync

import "unsafe"

// weaveEnabled is false in ordinary builds, so every `if weaveEnabled { ... }`
// hook is dead code the compiler removes before the inliner even prices the
// function. This is how internal/race keeps -race instrumentation free
// (race.Enabled), and it is what keeps RWMutex.RLock/RUnlock and Once.Do
// inlinable: a *call* to an empty function still costs 57 in the inline budget,
// which by itself pushed RLock/RUnlock over it.
const weaveEnabled = false

// weaveSchedPoint is never reached in ordinary builds (see weaveEnabled); it
// exists so the hook call sites still type-check.
//
//go:nosplit
func weaveSchedPoint(op uint8, obj unsafe.Pointer) {}
