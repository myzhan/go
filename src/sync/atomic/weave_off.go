// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !weave

package atomic

import "unsafe"

// weaveEnabled is false in ordinary builds, so every `if weaveEnabled { ... }`
// hook in type.go is dead code the compiler removes before the inliner even
// prices the method — the same trick internal/race uses for -race (race.Enabled).
// An empty function body is not enough: a *call* to it still costs 57 in the
// inline budget, and since the typed atomic methods are themselves inlined into
// hot callers (sync.RWMutex.RLock, ...), that cost leaked out and pushed those
// callers over their own budget.
const weaveEnabled = false

// weaveAtomic is never reached in ordinary builds (see weaveEnabled); it exists so
// the hook call sites still type-check.
//
//go:nosplit
func weaveAtomic(op uint8, addr unsafe.Pointer) {}
