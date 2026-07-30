// Package unsupported collects REAL concurrency bugs that weave currently MISSES,
// one bug per file, as executable documentation of where its model stops. Each is
// an ordinary
// testing/synctest test that is EXPECTED TO PASS under
// `../../bin/go test -weave ./...` — the pass is the false negative, and every
// file explains why weave cannot see the bug and what does catch it (usually
// `-race`).
//
// Cases weave handles — bugs it finds, plus correct code it certifies — live in
// ../supported.
package unsupported
