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
//go:linkname weaveGloballyActive runtime.weaveGloballyActive
var weaveGloballyActive uint32

//go:linkname runtime_weaveSchedPoint runtime.weaveSchedPoint
func runtime_weaveSchedPoint(op uint8, addr unsafe.Pointer)

//go:nosplit
func weaveAtomic(op uint8, addr unsafe.Pointer) {
	if weaveGloballyActive != 0 {
		runtime_weaveSchedPoint(op, addr)
	}
}
