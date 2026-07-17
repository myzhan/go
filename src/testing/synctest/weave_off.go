// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !weave

package synctest

import "testing"

// weaveExplore is a no-op unless the program is built with -weave. Returning
// false makes synctest.Test fall through to its ordinary single-run behavior.
func weaveExplore(*testing.T, func(*testing.T)) bool { return false }
