# Design a deterministic concurrency testing framework for Go

**Author:** weave contributors  
**Status:** Draft  

## Problem

Concurrent Go code is hard to test reliably. The Go scheduler is non-deterministic, so a bug can hide through thousands of test runs and then fail unpredictably in production. Existing tools do not fully solve this:

- `-race` only observes the single execution that ran. It misses schedule-dependent logic bugs in correctly-synchronized code, and it can miss races if the unlucky interleaving never occurs.
- `testing/synctest` provides deterministic time and isolation, but does not systematically explore interleavings.
- Manual tricks like `time.Sleep` or `runtime.Gosched` are fragile and unreliable.

The result is a class of heisenbugs that developers cannot confidently catch or reproduce.

## Proposal

Design a deterministic concurrency testing framework for Go. The framework runs ordinary concurrent unit tests inside a closed bubble, takes over goroutine scheduling at synchronization points, and systematically explores the relevant interleavings. When it finds a panic, deadlock, leak, or invariant violation, it reports a reproducible trace and a replay seed.

This requires cooperation across three parts of the toolchain:

- **Runtime:** pause and resume goroutines at synchronization points inside the test bubble, without affecting normal execution.
- **Compiler:** provide an opt-in instrumentation mode that turns shared-memory accesses and atomic operations into observable scheduling points.
- **Testing API:** expose the framework through a small package such as `testing/weave`, with an entry point that runs the model repeatedly and reports failures with replay seeds.

The intended developer experience is:

```go
func TestLostUpdate(t *testing.T) {
    weave.Test(t, func() {
        x := 0
        go func() { x++ }()
        go func() { x++ }()
        weave.Wait()
        if x != 2 { t.Errorf("x = %d, want 2", x) }
    })
}
```

Run with:

```sh
go test -weave
```

A failing report includes a goroutine legend mapped to creation sites, the failing schedule, and a replay seed:

```
weave: found failing interleaving after 3 schedule(s):
  goroutines:
    g0: model root
    g1: TestLostUpdate.func1 (example_test.go:12)
    g2: TestLostUpdate.func2 (example_test.go:13)
  schedule:
    1: g0 write example_test.go:11
    2: g1 read  example_test.go:12
    3: g2 read  example_test.go:13
    ...
reproduce with: WEAVE_REPLAY=0.0.0.1.1.0 go test -run TestLostUpdate
```

The user writes ordinary concurrent Go code, runs one command, and gets a deterministic, reproducible trace for any bug the framework discovers.
