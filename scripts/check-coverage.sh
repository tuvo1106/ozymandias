#!/usr/bin/env bash
# Runs the Go test suite with the race detector and coverage, then enforces
# the coverage gates from docs/plan/testing.md §1:
#
#   * TOTAL      — whole module, excluding cmd/*/main.go (pure wiring).
#   * per-package minimums for the critical packages, read from
#     scripts/coverage-thresholds.txt.
#
# A package with no statements yet (a doc.go-only stub) has nothing to cover
# and is skipped; a package with statements but no tests reports 0% and fails
# any gate that applies to it — which is the point.
#
# Usage: scripts/check-coverage.sh        (env: COVERPROFILE=coverage.out)
set -euo pipefail
cd "$(dirname "$0")/.."

module=$(go list -m)
profile=${COVERPROFILE:-coverage.out}
thresholds=scripts/coverage-thresholds.txt
testlog=$(mktemp)
filtered=$(mktemp)
trap 'rm -f "$testlog" "$filtered"' EXIT

# -timeout: the default is 10 minutes per package, and internal/tsdb/db is a
# whole-database package whose crash loop, stress test and 40 MiB log-roll
# recovery tests cost ~5 minutes under -race on a fast SSD. A 2-core CI runner
# with a slower disk took 600.06s and was killed mid-run by a budget nobody had
# thought about. Raise it rather than shrink the tests: the size is what lets
# them reach the bugs (docs/notes/M2.md).
go test -race -timeout 25m -covermode=atomic -coverprofile="$profile" ./... | tee "$testlog"

fail=0

# --- total, minus cmd/*/main.go ------------------------------------------------
grep -vE "^${module}/cmd/[^/]+/main\.go:" "$profile" >"$filtered"
total=$(go tool cover -func="$filtered" | awk '/^total:/ { sub(/%/, "", $NF); print $NF }')
total_min=$(awk '$1 == "TOTAL" { print $2 }' "$thresholds")
if awk -v t="$total" -v m="$total_min" 'BEGIN { exit !(t + 0 < m + 0) }'; then
  echo "✗ total coverage ${total}% is below ${total_min}%"
  fail=1
else
  echo "✓ total coverage ${total}% (gate ${total_min}%)"
fi

# --- per-package gates ----------------------------------------------------------
# `go test -cover` prints one line per package, e.g.
#   ok  	github.com/.../internal/clock	0.2s	coverage: 91.3% of statements
#   	github.com/.../internal/foo		coverage: 0.0% of statements   (no tests)
while read -r pattern min; do
  [[ -z "$pattern" || "$pattern" == \#* || "$pattern" == "TOTAL" ]] && continue
  while read -r pkg pct; do
    rel=${pkg#"$module"/}
    # shellcheck disable=SC2053  # the pattern is a deliberate glob
    [[ $rel == $pattern ]] || continue
    if awk -v p="$pct" -v m="$min" 'BEGIN { exit !(p + 0 < m + 0) }'; then
      echo "✗ ${rel}: ${pct}% is below ${min}% (${pattern})"
      fail=1
    else
      echo "✓ ${rel}: ${pct}% (gate ${min}%)"
    fi
  done < <(sed -nE 's/.*[[:space:]]('"${module//\//\\/}"'[^[:space:]]*)[[:space:]].*coverage: ([0-9.]+)% of statements.*/\1 \2/p' "$testlog")
done <"$thresholds"

exit "$fail"
