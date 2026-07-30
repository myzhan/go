// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build weave

package synctest_test

import (
	"internal/testenv"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
)

// t.Skip inside a model must skip the test, not be reported as a failing
// interleaving. It leaves via runtime.Goexit exactly like t.Fatal, and until ADR D21
// weave could not tell them apart — a bug that survived precisely because the existing
// skip tests opt out under -weave (runTest's underWeave check), so nothing exercised
// this path automatically. Forks a child the way runTest does, but asserts the -weave
// outcome rather than the plain-synctest output.
func TestWeaveSkipSkipsTheTest(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		synctest.Test(t, func(t *testing.T) {
			t.Skip("model decided to skip")
		})
		t.Error("unreachable: the skip above should have ended this test")
		return
	}
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t),
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "GO_WANT_HELPER_PROCESS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("a model that skips itself must not fail the test; child exited with %v:\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- SKIP") {
		t.Errorf("expected the test to be reported as skipped, got:\n%s", out)
	}
	if strings.Contains(string(out), "found failing interleaving") {
		t.Errorf("a skip was reported as a failing interleaving:\n%s", out)
	}
	if !strings.Contains(string(out), "model decided to skip") {
		t.Errorf("the skip reason should still reach the output:\n%s", out)
	}
}
