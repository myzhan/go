// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

package weave

// builtWithWeave reports whether this test binary was built with -weave (memory
// instrumentation). A failure found with memory accesses as scheduling points is
// only reproducible under the same build mode, so the reproduce command must
// carry -weave.
const builtWithWeave = true
