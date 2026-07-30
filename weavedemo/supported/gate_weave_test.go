//go:build weave

package supported

// builtWithWeave is true when the package is built with -weave (memory
// instrumentation). The GC-preemption stress test skips in this mode because
// instrumenting every memory access explodes its state space to a test timeout;
// it does not need -weave (it exercises the run token under GC preemption, not
// data races).
const builtWithWeave = true
