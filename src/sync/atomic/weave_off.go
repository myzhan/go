// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !weave

package atomic

import "unsafe"

// weaveAtomic is a no-op in ordinary (non -weave) builds. It is empty so the
// compiler inlines it away, leaving the typed atomic methods exactly as fast and
// as inlinable as before — weave instrumentation costs nothing when weave is off.
//
//go:nosplit
func weaveAtomic(op uint8, addr unsafe.Pointer) {}
