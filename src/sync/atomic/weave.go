// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic

// weave operation codes, kept in sync with runtime weaveOp. They are used by the
// typed methods in type.go to identify the atomic operation at a scheduling
// point. The actual hook (weaveAtomic) is build-tagged: a real scheduling-point
// call under -weave (weave_on.go) and an empty no-op otherwise (weave_off.go), so
// ordinary builds keep the atomic methods inlinable at zero cost.
const (
	weaveOpAtomicLoad  = 20
	weaveOpAtomicStore = 21
	weaveOpAtomicRMW   = 22
)
