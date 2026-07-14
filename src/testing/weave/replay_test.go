// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"strings"
	"testing"
)

// The reproduce command must be a single safe shell line: a select seed contains
// '|', which must be quoted so the shell does not treat it as a pipe, and the
// -run pattern must be anchored/quoted. It must also preserve the build mode
// (-weave iff built with it), since the two modes explore different schedules.
func TestReplayCommandFormat(t *testing.T) {
	cmd := replayCommand("0.1.2|0", "TestFoo/bar")

	if !strings.Contains(cmd, "WEAVE_REPLAY='0.1.2|0'") {
		t.Errorf("seed with '|' must be single-quoted; got: %s", cmd)
	}
	if !strings.Contains(cmd, "-run '^TestFoo/bar$'") {
		t.Errorf("-run pattern must be anchored and quoted; got: %s", cmd)
	}
	if builtWithWeave {
		if !strings.Contains(cmd, "go test -weave -run") {
			t.Errorf("a -weave build must reproduce with -weave; got: %s", cmd)
		}
	} else {
		if strings.Contains(cmd, "-weave") {
			t.Errorf("a non-weave build must not add -weave; got: %s", cmd)
		}
	}
	t.Logf("replay command: %s", cmd)
}
