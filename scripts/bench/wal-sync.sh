#!/usr/bin/env bash
# L12 follow-up (docs/benchmarks.md): what the stores' sustained ingest is bound by. Runs the
# series throughput of intake.sh with `storage.wal_sync_on_append` on (the default) and off, in
# both pipeline modes. Run `make build` first.
#
#   scripts/bench/wal-sync.sh
#   BENCH_DURATION=30s scripts/bench/wal-sync.sh
#   BENCH_SYNC=true BENCH_BATCH=500 scripts/bench/wal-sync.sh   # only the default setting, 500 per request
#
# Off is not a recommendation: after a crash of the OS or the power, the points the WAL had not yet
# synced are gone, and in queued mode the consumer has by then committed them off the queue (see
# docs/benchmarks.md). It is here to show how much of the ceiling is the fsync.
source "$(dirname "${BASH_SOURCE[0]}")/../chaos/lib.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

DURATION=${BENCH_DURATION:-15s}
WORKERS=${BENCH_WORKERS:-8}
BATCH=${BENCH_BATCH:-100}
SYNCS=${BENCH_SYNC:-true false}

echo "# $(uname -sm), $(sysctl -n hw.ncpu 2>/dev/null || nproc) cpus, $(date -u +%FT%TZ)"
for mode in direct queued; do
  for sync in $SYNCS; do
    echo
    echo "== pipeline.mode=$mode storage.wal_sync_on_append=$sync"
    rm -rf "$WORK/data"
    CHAOS_MODE=$mode start_ozyd OZY_STORAGE_WAL_SYNC_ON_APPEND=$sync
    wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not become ready ($mode, wal_sync_on_append=$sync)"
    # Captured, not piped: a pipeline that ends in grep exits non-zero when loadgen's report format
    # drifts, which would kill the whole matrix with no message; a failed run is `fail`ed by throughput_run.
    out=$(throughput_run -signal series -workers "$WORKERS" -batch "$BATCH" -duration "$DURATION")
    grep -E "acknowledged \(|drained|NOT drained|first failure" <<<"$out" || echo "$out"
    kill -TERM "$OZYD_PID" 2>/dev/null || true
    wait "$OZYD_PID" 2>/dev/null || true
    OZYD_PID=""
  done
done
