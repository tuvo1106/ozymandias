#!/usr/bin/env bash
# Shared by scripts/bench/*.sh. Source it after scripts/chaos/lib.sh; do not run it.

# throughput_run <label> <loadgen args...>: runs an intake-throughput scenario, prints its report,
# then times how long the queues take to drain and derives the rate the stores sustained.
#
# A 202 in queued mode is "durably queued", not "stored", so the acknowledged rate alone is the
# front door's speed. The sustained rate is acknowledged items over (run time + drain time).
throughput_run() {
  local out
  out=$("$BIN/loadgen" -scenario intake-throughput -ozyd "$OZYD_URL" "$@") || { echo "$out"; fail "throughput run failed"; }
  echo "$out"
  local t0 t1 limit=120
  t0=$(python3 -c 'import time; print(time.time())')
  until queues_drained; do
    kill -0 "$OZYD_PID" 2>/dev/null || fail "ozyd died while its queues were draining"
    t1=$(python3 -c 'import time; print(time.time())')
    if python3 -c "import sys; sys.exit(0 if $t1 - $t0 > $limit else 1)"; then
      echo "  queues had NOT drained ${limit}s after the run ended (or /debug/vars was unreadable)"
      return 0
    fi
    sleep 0.2
  done
  t1=$(python3 -c 'import time; print(time.time())')
  OUT="$out" T0=$t0 T1=$t1 python3 - <<'PY'
import os, re
out, drain = os.environ["OUT"], float(os.environ["T1"]) - float(os.environ["T0"])
secs = float(re.search(r"per request, ([0-9.]+)s", out).group(1))
acked = float(re.search(r"requests/s, ([0-9.]+) ", out).group(1))
print("  queues drained %.1fs after the run ended; stored rate over run+drain: %.0f/s" % (drain, acked * secs / (secs + drain)))
PY
}
