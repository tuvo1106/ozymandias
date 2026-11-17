#!/usr/bin/env bash
# L12 (docs/plan/testing.md): the queue's group-commit window against ack latency and
# throughput, queued mode. `pipeline.queue.sync_interval` is how long a topic waits after the
# first unsynced record before it fsyncs, so one fsync covers every request that arrived in
# the window. Negative syncs as soon as the syncer wakes. Reports the rate the stores sustained
# (acknowledged over run + drain), not only what the queue accepted. Run `make build` first.
#
#   scripts/bench/group-commit.sh
#   BENCH_DURATION=30s BENCH_INTERVALS="-1ms 1ms 5ms" scripts/bench/group-commit.sh
source "$(dirname "${BASH_SOURCE[0]}")/../chaos/lib.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

N=${BENCH_N:-200}
DURATION=${BENCH_DURATION:-15s}
WORKERS=${BENCH_WORKERS:-8}
BATCH=${BENCH_BATCH:-100}

echo "# $(uname -sm), $(sysctl -n hw.ncpu 2>/dev/null || nproc) cpus, $(date -u +%FT%TZ)"
for si in ${BENCH_INTERVALS:--1ms 1ms 5ms 20ms}; do
  echo
  echo "== pipeline.queue.sync_interval=$si"
  rm -rf "$WORK/data"
  CHAOS_MODE=queued start_ozyd OZY_PIPELINE_QUEUE_SYNC_INTERVAL="$si"
  wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not become ready at sync_interval=$si"
  "$BIN/loadgen" -scenario intake-latency -ozyd "$OZYD_URL" -n "$N" | grep acknowledged || fail "latency run failed ($si)"
  out=$(throughput_run -signal series -workers "$WORKERS" -batch "$BATCH" -duration "$DURATION")
  grep -E "acknowledged \(|request latency|drained|NOT drained|first failure" <<<"$out" || echo "$out"
  kill -TERM "$OZYD_PID" 2>/dev/null || true
  wait "$OZYD_PID" 2>/dev/null || true
  OZYD_PID=""
done
