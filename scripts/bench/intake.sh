#!/usr/bin/env bash
# L12 (docs/plan/testing.md): how long a point takes to become queryable, and how much one
# ozyd takes in, with pipeline.mode direct and then queued, on this machine, in a temp
# directory, on loopback. The numbers are for comparing the two modes with each other, run
# back to back; they are not a capacity claim. Run `make build` first.
#
#   scripts/bench/intake.sh
#   BENCH_DURATION=30s BENCH_N=500 BENCH_WORKERS=16 scripts/bench/intake.sh
#
# Reuses the chaos drills' setup (own ports, own data dir, cleanup on exit).
source "$(dirname "${BASH_SOURCE[0]}")/../chaos/lib.sh"

N=${BENCH_N:-300}
DURATION=${BENCH_DURATION:-15s}
WORKERS=${BENCH_WORKERS:-8}
BATCH=${BENCH_BATCH:-100}

echo "# $(uname -sm), $(sysctl -n hw.ncpu 2>/dev/null || nproc) cpus, $(date -u +%FT%TZ)"
for mode in direct queued; do
  echo
  echo "== pipeline.mode=$mode"
  rm -rf "$WORK/data"
  CHAOS_MODE=$mode start_ozyd
  wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not become ready in $mode mode"
  "$BIN/loadgen" -scenario intake-latency -ozyd "$OZYD_URL" -n "$N" || fail "latency run failed ($mode)"
  for signal in series logs; do
    "$BIN/loadgen" -scenario intake-throughput -ozyd "$OZYD_URL" -signal "$signal" \
      -workers "$WORKERS" -batch "$BATCH" -duration "$DURATION" || fail "$signal throughput run failed ($mode)"
    # A 202 in queued mode is "durably queued", not "stored": the rate above is only sustainable
    # if the consumers kept up. How long the backlog took to drain is the other half of it.
    ticks=0
    until queues_drained; do
      sleep 0.2
      ticks=$((ticks + 1))
      if ((ticks > 600)); then echo "  queues had NOT drained 120s after the run ended"; break; fi
    done
    ((ticks > 600)) || echo "  queues drained $((ticks / 5)).$(((ticks % 5) * 2))s after the run ended"
  done
  kill -TERM "$OZYD_PID" 2>/dev/null || true
  wait "$OZYD_PID" 2>/dev/null || true
  OZYD_PID=""
done
