#!/usr/bin/env bash
# L12 follow-up (docs/benchmarks.md): what the stores' sustained ingest is bound by. Runs the
# series throughput of intake.sh with `storage.wal_sync_on_append` on (the default) and off, in
# both pipeline modes. Run `make build` first.
#
#   scripts/bench/wal-sync.sh
#   BENCH_DURATION=30s scripts/bench/wal-sync.sh
#
# Off is NOT a setting to run with: a crash loses what the WAL had not yet synced, and in queued
# mode the consumer has already committed it. It is here to show how much of the ceiling is the
# fsync.
source "$(dirname "${BASH_SOURCE[0]}")/../chaos/lib.sh"
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

DURATION=${BENCH_DURATION:-15s}
WORKERS=${BENCH_WORKERS:-8}
BATCH=${BENCH_BATCH:-100}

echo "# $(uname -sm), $(sysctl -n hw.ncpu 2>/dev/null || nproc) cpus, $(date -u +%FT%TZ)"
for mode in direct queued; do
  for sync in true false; do
    echo
    echo "== pipeline.mode=$mode storage.wal_sync_on_append=$sync"
    rm -rf "$WORK/data"
    CHAOS_MODE=$mode start_ozyd OZY_STORAGE_WAL_SYNC_ON_APPEND=$sync
    wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not become ready ($mode, wal_sync_on_append=$sync)"
    throughput_run -signal series -workers "$WORKERS" -batch "$BATCH" -duration "$DURATION" \
      | grep -E "acknowledged \(|drained|NOT drained|first failure"
    kill -TERM "$OZYD_PID" 2>/dev/null || true
    wait "$OZYD_PID" 2>/dev/null || true
    OZYD_PID=""
  done
done
