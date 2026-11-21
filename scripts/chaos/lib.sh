#!/usr/bin/env bash
# Shared by the chaos drills (docs/plan/M7-hardening.md §6). Source it; do not run it.
#
# A drill starts its own ozyd (and agent) from ./bin on loopback ports of its own,
# in a temp directory, breaks something, and asserts what must still be true. It
# prints "✓ <what>" for each assertion and exits non-zero on the first failure, so a
# CI step is just `scripts/chaos/<drill>.sh`. Run `make build` first.
#
# bash 3.2 (macOS): no `wait -n`, no `mapfile`, no `timeout`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT=$(pwd)
BIN=$ROOT/bin

for b in ozyd agent loadgen; do
  [[ -x $BIN/$b ]] || { echo "chaos: $BIN/$b is missing; run \`make build\`" >&2; exit 2; }
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/ozy-chaos.XXXXXX")
# Ports are derived from the pid so two drills can run side by side; they are
# loopback only.
# Below 32768, where Linux hands out ephemeral ports: a drill's own curl or loadgen could
# otherwise be given the port its ozyd is about to listen on.
PORT_BASE=$((20000 + ($$ % 1200) * 10))
while (exec 3<>/dev/tcp/127.0.0.1/$PORT_BASE) 2>/dev/null; do PORT_BASE=$((PORT_BASE + 10)); done
OZYD_PORT=$PORT_BASE
AGENT_HTTP_PORT=$((PORT_BASE + 1))
AGENT_UDP_PORT=$((PORT_BASE + 2))
OZYD_URL=http://127.0.0.1:$OZYD_PORT
AGENT_URL=http://127.0.0.1:$AGENT_HTTP_PORT
OZYD_PID=""
AGENT_PID=""
BG_PIDS=""

# detach: a drill that mounts something redefines this; it runs before $WORK is removed
# (which can contain the mount point).
detach() { :; }

cleanup() {
  trap - EXIT INT TERM
  local p
  for p in $BG_PIDS $AGENT_PID $OZYD_PID; do
    [[ -n $p ]] && kill -TERM "$p" 2>/dev/null || true
  done
  sleep 0.3
  for p in $BG_PIDS $AGENT_PID $OZYD_PID; do
    [[ -n $p ]] && kill -KILL "$p" 2>/dev/null || true
  done
  detach
  if [[ -n ${CHAOS_KEEP:-} ]]; then echo "chaos: kept $WORK"; else rm -rf "$WORK"; fi
}
# INT and TERM must end the script, not only clean up: bash runs a trap and then carries on
# with the next command, which would keep asserting against a deleted $WORK.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass=0
ok() { echo "✓ $1"; pass=$((pass + 1)); }
fail() {
  echo "✗ $1" >&2
  for f in "$WORK"/ozyd.log "$WORK"/agent.log; do
    [[ -f $f ]] && { echo "--- tail of $(basename "$f") ---" >&2; tail -n 15 "$f" >&2; }
  done
  exit 1
}

# start_ozyd [VAR=value ...]: ozyd (OZYD_BIN, default ./bin/ozyd) on its own data dir, queued unless a caller says
# otherwise. Config is the environment's, so a drill states exactly what it changes.
start_ozyd() {
  env OZY_HTTP_ADDR=127.0.0.1:$OZYD_PORT OZY_DATA_DIR="$WORK/data" OZY_HOSTNAME=chaos \
    OZY_HTTP_SHUTDOWN_TIMEOUT=5s OZY_PIPELINE_MODE=${CHAOS_MODE:-queued} "$@" \
    "${OZYD_BIN:-$BIN/ozyd}" >>"$WORK/ozyd.log" 2>&1 &
  OZYD_PID=$!
}

wait_ready() { # <url> [seconds]
  local i n=$(( ${2:-20} * 10 ))
  for ((i = 0; i < n; i++)); do
    curl -fsS --max-time 2 "$1" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}

# kill9_ozyd: SIGKILL, no flush, no goodbye. Waits for the process to be gone.
kill9_ozyd() {
  kill -KILL "$OZYD_PID" 2>/dev/null || true
  wait "$OZYD_PID" 2>/dev/null || true
  OZYD_PID=""
}

start_agent() { # [VAR=value ...]
  env OZY_AGENT_HTTP_ADDR=127.0.0.1:$AGENT_HTTP_PORT OZY_AGENT_STATSD_ADDR=127.0.0.1:$AGENT_UDP_PORT \
    OZY_AGENT_INTAKE_URL=$OZYD_URL OZY_AGENT_HOSTNAME=chaos OZY_AGENT_STATE_DIR="$WORK/agent" \
    OZY_AGENT_COLLECTORS_DOCKER_ENABLED=false "$@" \
    "$BIN/agent" >>"$WORK/agent.log" 2>&1 &
  AGENT_PID=$!
}

# queues_drained: every queue has been drained by its consumer. verify must not judge the
# stores while records that were replayed after a kill are still on their way to them: a
# duplicate they would produce lands after the check.
queues_drained() {
  local lag
  lag=$(curl -fsS --max-time 3 "$OZYD_URL/debug/vars" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(int(sum(m["value"] for m in d["metrics"] if m["name"] == "ozy.queue.lag_records")))' 2>/dev/null) || return 1
  [[ $lag = 0 ]]
}

# in_background <command...>: run something that must be stopped with the drill.
in_background() { "$@" & BG_PIDS="$BG_PIDS $!"; LAST_BG=$!; }

# metric <base-url> <name>: the sum of a self-metric's values over every tag set, from
# /debug/vars, or 0 when it does not exist (yet).
metric() {
  curl -fsS --max-time 3 "$1/debug/vars" 2>/dev/null | python3 -c '
import json, sys
name = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print(0); sys.exit()
print(int(sum(m["value"] for m in d.get("metrics", []) if m["name"] == name and m.get("value") is not None)))
' "$2"
}

# stored_sum <metric> [window-seconds [interval-seconds]]: the sum of every bucket of a stored
# metric. A gauge is averaged inside a bucket, so counting gauge points takes an interval short
# enough that no bucket holds two.
stored_sum() {
  local now; now=$(date +%s)
  curl -sS --max-time 30 "$OZYD_URL/api/v1/query?metric=$1&agg=sum&from=$((now - ${2:-3600}))&to=$((now + 60))&interval=${3:-10}" |
    python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(0); sys.exit()
if d.get("status") != "ok":
    # Not zero silently: an error read as "nothing stored" sent this drill hunting for
    # lost data that was never lost (a query over 1000 series is refused).
    print("stored_sum: " + str(d.get("error")), file=sys.stderr); print(0); sys.exit()
print(int(sum(p[1] for s in d.get("series", []) for p in s["points"] if p[1] is not None)))
'
}

rss_mb() { echo $(( $(ps -o rss= -p "$1" 2>/dev/null || echo 0) / 1024 )); }

# eventually <seconds> <description> <shell expression>: re-evaluates the expression
# (with eval, so it is re-run each time, not expanded once) until it succeeds.
eventually() {
  local n=$1 desc=$2 expr=$3 i
  for ((i = 0; i < n * 2; i++)); do
    eval "$expr" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  fail "$desc"
}
