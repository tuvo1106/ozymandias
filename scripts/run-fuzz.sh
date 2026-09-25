#!/usr/bin/env bash
# Runs every Go fuzz target for FUZZTIME each (docs/plan/testing.md L4).
# CI uses 30s; `make fuzz-long` uses 10m. `go test -fuzz` accepts exactly one
# target per invocation, hence the loop.
#
# Usage: FUZZTIME=30s scripts/run-fuzz.sh
set -euo pipefail
cd "$(dirname "$0")/.."

fuzztime=${FUZZTIME:-30s}
found=0
while IFS=: read -r file name; do
  found=1
  pkg="./$(dirname "$file")"
  echo "── $pkg $name ($fuzztime)"
  go test "$pkg" -run '^$' -fuzz "^${name}\$" -fuzztime "$fuzztime"
done < <(grep -rEo '^func (Fuzz[A-Za-z0-9_]*)' --include='*_test.go' . |
  sed -E 's/^\.\/(.*):func (Fuzz[A-Za-z0-9_]*)$/\1:\2/' | sort)

[[ $found -eq 1 ]] || echo "no fuzz targets yet"
