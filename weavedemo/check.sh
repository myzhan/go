#!/bin/sh
# check.sh verifies that every demo behaves as its own doc comment claims.
#
# The demos are deliberately a mix of expected-FAIL (weave found a real bug) and
# expected-PASS cases, so a plain `go test` exit status cannot validate them. This
# script derives the expectation from the source — a test whose doc comment says
# "EXPECTED TO FAIL" must fail under -weave, and every other test must pass or skip
# — then runs the suite and diffs the two. Without it, a regression that silently
# stops finding one of these bugs would go unnoticed (weavedemo is a separate
# module and is not covered by any std test).
#
# Usage: cd weavedemo && ./check.sh          (uses ../bin/go)
#        GO=/path/to/go ./check.sh
set -eu

GO=${GO:-../bin/go}
cd "$(dirname "$0")"

expected=$(mktemp)
actual=$(mktemp)
trap 'rm -f "$expected" "$actual"' EXIT

# Expected outcomes, from each test's doc comment. awk accumulates the comment
# block immediately above a func and checks it for the EXPECTED TO FAIL marker;
# any other line resets the block, so a package doc never attaches to a test.
awk '
  /^\/\// { block = block $0 "\n"; next }
  /^func Test/ {
    name = $2; sub(/\(.*/, "", name)
    print name, (block ~ /EXPECTED TO FAIL/) ? "FAIL" : "OK"
    block = ""; next
  }
  { block = "" }
' supported/*_test.go unsupported/*_test.go | sort >"$expected"

# Actual outcomes. SKIP counts as OK: a demo may legitimately skip under -weave
# (gcpreempt_test.go does).
"$GO" test -count=1 -weave -v ./supported/ ./unsupported/ 2>&1 |
	awk '
    /^ *--- FAIL: Test/ { name = $3; sub(/\(.*/, "", name); print name, "FAIL" }
    /^ *--- (PASS|SKIP): Test/ { name = $3; sub(/\(.*/, "", name); print name, "OK" }
  ' | sort >"$actual"

if diff -u "$expected" "$actual"; then
	printf 'weavedemo: %d demos behave as documented\n' "$(wc -l <"$expected" | tr -d ' ')"
else
	echo
	echo 'weavedemo: MISMATCH between documented and actual behavior (left = documented, right = observed).'
	echo 'A test that stopped failing means weave lost the ability to find that bug.'
	exit 1
fi
