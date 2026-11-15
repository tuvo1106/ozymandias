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

DRILLS=${CHAOS_DRILLS:-kill9 ozyd-down agent-down udp-flood clock-jump cardinality-bomb disk-full}
failed=""
for d in $DRILLS; do
  echo "=== $d"
  if bash "scripts/chaos/$d.sh" 2>&1 | grep -v '^time='; [[ ${PIPESTATUS[0]} -eq 0 ]]; then :; else failed="$failed $d"; fi
done
if [[ -n $failed ]]; then echo "FAILED:$failed"; exit 1; fi
echo "all drills passed"
