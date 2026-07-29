// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// weave scheduling-point support for Mutex.
//
// Unlike the sync package's hooks (which are build-tagged, see sync/weave_on.go),
// Mutex's hook is always compiled and gated at run time by weaveGloballyActive, so
// internal/weave can explore mutex interleavings under a plain `go test` with no
// -weave build. That is affordable here and only here: Lock/TryLock/Unlock stay
// within the inliner's budget with the hook present, whereas RWMutex.RLock/RUnlock
// and Once.Do sit exactly at their budget upstream and lose inlining if anything
// is added. See .claude/weave/design.md D8 and D20.
//
//go:linkname weaveGloballyActive runtime.weaveGloballyActive
var weaveGloballyActive uint32

// weave operation codes, kept in sync with runtime weaveOp (see runtime/weave.go);
// internal/weave/optab_test.go asserts they agree.
const (
	weaveOpLock   = 3
	weaveOpUnlock = 4
)

//go:linkname runtime_weaveSchedPoint runtime.weaveSchedPoint
func runtime_weaveSchedPoint(op uint8, addr unsafe.Pointer)
