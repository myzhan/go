// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !weave

package synctest_test

// underWeave is false in ordinary (non -weave) builds; see underweave_on_test.go.
const underWeave = false
