// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"internal/weave"
	"strings"
	"testing"
	"time"
)

// The per-test wall-clock backstop: WEAVE_TIMEOUT unset uses the default; a
// valid duration overrides it; 0 disables the limit; garbage falls back to the
// default.
func TestResolveTimeout(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultTimeout},
		{"5s", 5 * time.Second},
		{"2m", 2 * time.Minute},
		{"0", 0},
		{"not-a-duration", defaultTimeout},
	}
	for _, c := range cases {
		if got := resolveTimeout(c.env); got != c.want {
			t.Errorf("resolveTimeout(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// A replayed result must fail the test unless it ran cleanly to completion. In
// particular a Truncated replay (capacity overflow) is inconclusive and must not
// be reported as a silent pass.
func TestReplayInconclusive(t *testing.T) {
	if replayInconclusive(weave.Result{}) {
		t.Errorf("a clean replay result must not be inconclusive")
	}
	for _, res := range []weave.Result{
		{Failed: true},
		{Deadlock: true},
		{Truncated: true},
	} {
		if !replayInconclusive(res) {
			t.Errorf("result %+v must be treated as inconclusive (not a silent pass)", res)
		}
	}
}

// The reproduce command must be a single safe shell line: a select seed contains
// '|', which must be quoted so the shell does not treat it as a pipe; the -run
// pattern must escape regexp metacharacters per name segment; and -weave is added
// exactly when the failure involved memory instrumentation (a read/write in the
// trace), reflecting the actual build mode rather than a build tag.
func TestReplayCommandFormat(t *testing.T) {
	// Channel/mutex-only failure: no memory op in the trace, so no -weave.
	chanTrace := []weave.Step{{Op: "chan send"}, {Op: "chan recv"}}
	cmd := replayCommand("0.1.2|0", "TestFoo/a+b", chanTrace)
	if !strings.Contains(cmd, "WEAVE_REPLAY='0.1.2|0'") {
		t.Errorf("seed with '|' must be single-quoted; got: %s", cmd)
	}
	if !strings.Contains(cmd, `-run '^TestFoo$/^a\+b$'`) {
		t.Errorf("-run must escape regexp metacharacters per segment; got: %s", cmd)
	}
	if strings.Contains(cmd, "-weave") {
		t.Errorf("a channel/mutex-only failure must not add -weave; got: %s", cmd)
	}

	// Memory-race failure: the trace has a read/write, so the command must carry
	// -weave (the failure is only reproducible with instrumentation).
	memTrace := []weave.Step{{Op: "read"}, {Op: "write"}}
	cmd2 := replayCommand("0.0", "TestMem", memTrace)
	if !strings.Contains(cmd2, "go test -weave -run") {
		t.Errorf("a memory-race failure must reproduce with -weave; got: %s", cmd2)
	}
	t.Logf("replay commands: %q / %q", cmd, cmd2)
}
