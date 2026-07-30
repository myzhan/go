// Package supported collects the cases that weave HANDLES CORRECTLY, one topic per
// file. Every file is an ordinary testing/synctest test — no weave-specific API —
// and each says at the top which of two kinds it is:
//
//   - EXPECTED TO FAIL under `../../bin/go test -weave ./...`: a real concurrency
//     bug that weave finds. The failure IS the demo: it prints the interleaving,
//     a goroutine legend and a WEAVE_REPLAY seed. (Most files here.)
//   - EXPECTED TO PASS: correct code that weave explores exhaustively without
//     reporting anything — the `correct_*` files, plus gcpreempt_test.go. A bug
//     finder that cannot certify correct code is not useful, so these guard
//     against false positives.
//
// Cases that are real bugs but that weave currently MISSES live in ../unsupported.
//
// Behavior WITHOUT -weave (a single synctest schedule) is deliberately not
// specified: some of these bugs are missed, some are hit, some are flaky. That
// unreliability is exactly the problem weave solves, so the cases are written
// according to their own correctness and are never reshaped to suit the single-run
// scheduler. Concretely, as of this writing a plain `go test ./supported/` fails
// TestChannelOrderAssumption and TestWaitGroupAddInGoroutine every time, is flaky on
// TestSelectPriorityAssumption, and ABORTS THE TEST BINARY at
// TestTimeoutGoroutineLeak (a deadlocked synctest bubble is a panic, not a test
// failure — see that file). Under -weave every one of them is a deterministic,
// reproducible report.
package supported
