#!/usr/bin/env bash
# Runs every drill, one after another, and says which failed. Not sourced: each drill
# is its own process with its own temp dir and ports.
#
#   scripts/chaos/all.sh           quick settings: a few kills, short outages (about 3 minutes)
#   scripts/chaos/all.sh long      the acceptance settings of docs/plan/M7-hardening.md §9 (about 40 minutes)
#   CHAOS_DRILLS="kill9 disk-full" scripts/chaos/all.sh    only those
set -uo pipefail
cd "$(dirname "$0")/../.."

if [[ ${1:-} = long ]]; then
  export CHAOS_KILLS=50 CHAOS_DOWN=900 CHAOS_RATE=500000 CHAOS_FLOOD=60 CHAOS_SERIES=1000000
fi

DRILLS=${CHAOS_DRILLS:-kill9 ozyd-down agent-down udp-flood clock-jump cardinality-bomb disk-full slow-disk}
failed=""
skipped=""
for d in $DRILLS; do
  echo "=== $d"
  out=$(mktemp)
  bash "scripts/chaos/$d.sh" 2>&1 | grep -v '^time=' | tee "$out"
  if [[ ${PIPESTATUS[0]} -ne 0 ]]; then failed="$failed $d"; elif grep -q '^SKIP:' "$out"; then skipped="$skipped $d"; fi
  rm -f "$out"
done
if [[ -n $failed ]]; then echo "FAILED:$failed"; exit 1; fi
if [[ -n $skipped ]]; then echo "passed, but SKIPPED:$skipped (not run: this is not a pass for them)"; exit 0; fi
echo "all drills passed"
