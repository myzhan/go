// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package weave

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The weave operation codes are declared five times: as an iota block in
// runtime/weave.go (the source of truth) and in this package, and as hardcoded
// literals in the three packages that call runtime.weaveSchedPoint by linkname
// (sync, internal/sync, sync/atomic — they cannot import the runtime's unexported
// constants). Inserting an op in the middle of the runtime's block silently
// renumbers everything after it, which would misclassify transitions rather than
// fail to build. This test is the guard: it reads the constant blocks out of the
// source tree and asserts every table agrees.
//
// The convention the parser relies on is that each op constant is named
// weaveOp<Name> (runtime, sync*) or op<Name> (this package), so the tables are
// compared by <Name>.

// opTable is this package's table, built from the iota block in explore.go.
var opTable = map[string]uint8{
	"None":          opNone,
	"Read":          opRead,
	"Write":         opWrite,
	"Lock":          opLock,
	"Unlock":        opUnlock,
	"ChanSend":      opChanSend,
	"ChanRecv":      opChanRecv,
	"ChanClose":     opChanClose,
	"Select":        opSelect,
	"WaitGroupWait": opWaitGroupWait,
	"GoStart":       opGoStart,
	"GoExit":        opGoExit,
	"Preempt":       opPreempt,
	"WaitGroupAdd":  opWaitGroupAdd,
	"CondWait":      opCondWait,
	"CondSignal":    opCondSignal,
	"CondBroadcast": opCondBroadcast,
	"Once":          opOnce,
	"ChanSendNB":    opChanSendNB,
	"ChanRecvNB":    opChanRecvNB,
	"AtomicLoad":    opAtomicLoad,
	"AtomicStore":   opAtomicStore,
	"AtomicRMW":     opAtomicRMW,
	"RLock":         opRLock,
	"RUnlock":       opRUnlock,
	"ClockAdvance":  opClockAdvance,
}

// The synthetic clock participant's id must match the runtime's, or a clock
// transition would be attributed to a real goroutine (and vice versa).
func TestClockWidReserved(t *testing.T) {
	const want = 63 // runtime/weave.go: weaveClockWid
	if ClockWid != want {
		t.Fatalf("ClockWid = %d, runtime reserves %d", ClockWid, want)
	}
	if ClockWid > 63 {
		t.Fatalf("ClockWid = %d does not fit the 64-bit enabled mask", ClockWid)
	}
}

func TestOpCodesAgreeWithRuntime(t *testing.T) {
	goroot := testenv.GOROOT(t)

	// runtime/weave.go is the source of truth: a single `weaveOp = iota` block.
	rt := parseIotaOps(t, filepath.Join(goroot, "src", "runtime", "weave.go"))
	if len(rt) != len(opTable) {
		t.Errorf("runtime declares %d ops, internal/weave declares %d: %v vs %v",
			len(rt), len(opTable), sortedOpNames(rt), sortedOpNames(opTable))
	}
	for name, want := range rt {
		got, ok := opTable[name]
		if !ok {
			t.Errorf("op %q = %d in runtime/weave.go, missing from internal/weave", name, want)
			continue
		}
		if got != want {
			t.Errorf("op %q: internal/weave has %d, runtime/weave.go has %d", name, got, want)
		}
	}

	// The linkname callers hardcode the numbers, so they must match by value.
	for _, file := range []string{
		filepath.Join(goroot, "src", "sync", "weave.go"),
		filepath.Join(goroot, "src", "internal", "sync", "weave.go"),
		filepath.Join(goroot, "src", "sync", "atomic", "weave.go"),
	} {
		lits := parseLiteralOps(t, file)
		if len(lits) == 0 {
			t.Errorf("%s: found no weaveOp constants (renamed or moved?)", file)
		}
		for name, got := range lits {
			want, ok := rt[name]
			if !ok {
				t.Errorf("%s: op %q is not declared in runtime/weave.go", file, name)
				continue
			}
			if got != want {
				t.Errorf("%s: op %q = %d, runtime/weave.go has %d", file, name, got, want)
			}
		}
	}
}

// parseIotaOps reads the `weaveOp<Name>` iota block from a runtime source file and
// returns name -> value. It expects the block to start with a line declaring the
// first constant with `weaveOp = iota` and to end at the closing paren.
func parseIotaOps(t *testing.T, file string) map[string]uint8 {
	t.Helper()
	ops := map[string]uint8{}
	next := uint8(0)
	inBlock := false
	for _, line := range readLines(t, file) {
		code := strings.TrimSpace(stripComment(line))
		if !inBlock {
			if strings.HasPrefix(code, "weaveOpNone weaveOp = iota") {
				inBlock = true
				ops["None"] = next
				next++
			}
			continue
		}
		if code == ")" {
			break
		}
		if !strings.HasPrefix(code, "weaveOp") {
			continue // a comment line inside the block
		}
		ops[strings.TrimPrefix(strings.Fields(code)[0], "weaveOp")] = next
		next++
	}
	if !inBlock {
		t.Fatalf("%s: could not find the weaveOp iota block", file)
	}
	return ops
}

// parseLiteralOps reads `weaveOp<Name> = <n>` constant declarations, as used by the
// packages that hook into weave by linkname.
func parseLiteralOps(t *testing.T, file string) map[string]uint8 {
	t.Helper()
	ops := map[string]uint8{}
	for _, line := range readLines(t, file) {
		code := strings.TrimSpace(stripComment(line))
		if !strings.HasPrefix(code, "weaveOp") {
			continue
		}
		name, val, ok := strings.Cut(code, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		ops[strings.TrimPrefix(strings.TrimSpace(name), "weaveOp")] = uint8(n)
	}
	return ops
}

func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

func readLines(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	return strings.Split(string(b), "\n")
}

func sortedOpNames(m map[string]uint8) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	// insertion sort: this package avoids pulling in sort for a diagnostic
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
