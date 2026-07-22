// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

package synctest_test

// underWeave reports whether the test binary was built with -weave, so tests
// that verify plain-synctest behavior (testing-package output formatting,
// timer-heavy stress) can skip: under -weave, synctest.Test runs the closure
// under systematic exploration, which changes their observable behavior.
const underWeave = true
