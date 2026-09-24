#!/usr/bin/env bash
# End-to-end smoke test against the running compose stack (docs/plan/testing.md
# L9). It grows with every milestone; each block below is labelled with the
# milestone that added it. Must stay under two minutes.
#
# Usage: make up && make smoke
#   OZY_URL (default http://localhost:9400)
#   AGENT_URL     (default http://localhost:8126)
set -euo pipefail
cd "$(dirname "$0")/.."

OZY_URL=${OZY_URL:-http://localhost:9400}
AGENT_URL=${AGENT_URL:-http://localhost:8126}
COMPOSE=(docker compose -f deploy/docker-compose.yml)

pass=0
check() { # <description> <command...>
  local desc=$1; shift
  if "$@" >/dev/null 2>&1; then
    echo "✓ $desc"; pass=$((pass + 1))
  else
    echo "✗ $desc"
    echo "  command: $*"
    echo "  --- recent logs ---"
    "${COMPOSE[@]}" logs --tail 20 2>&1 | sed 's/^/  /'
    exit 1
  fi
}
body_has() { curl -fsS --max-time 5 "$1" | grep -q -- "$2"; }
header_has() { curl -fsS --max-time 5 -o /dev/null -D - "$1" | tr -d '\r' | grep -qi -- "$2"; }
healthy() { [[ $(docker inspect -f '{{.State.Health.Status}}' "$("${COMPOSE[@]}" ps -q "$1")") == healthy ]]; }

# --- M0: both processes up, healthy, serving -----------------------------------
check "ozyd /healthz"                 body_has "$OZY_URL/healthz" '"component":"ozyd"'
check "agent /healthz"                     body_has "$AGENT_URL/healthz" '"component":"agent"'
check "ozyd container healthy"        healthy ozyd
check "agent container healthy"            healthy agent
check "agent tags data with the Mac's name" body_has "$AGENT_URL/healthz" "\"hostname\":\"${OZY_HOSTNAME:-$(hostname -s)}\""
check "agent forwards to ozyd by name" body_has "$AGENT_URL/healthz" '"intake_url":"http://ozyd:9400"'
check "ozyd self-metrics"             body_has "$OZY_URL/debug/vars" 'ozy.build.info'
check "UI index served"                    body_has "$OZY_URL/" '<div id="root">'
check "UI deep link falls back to index"   body_has "$OZY_URL/logs/live" '<div id="root">'
check "UI index is not cached"             header_has "$OZY_URL/" 'cache-control: no-cache'

# M0 acceptance: SIGTERM → clean exit (0) within the grace period.
for svc in agent ozyd; do
  cid=$("${COMPOSE[@]}" ps -q "$svc")
  start=$(date +%s)
  docker kill -s TERM "$cid" >/dev/null
  docker wait "$cid" >/tmp/ozymandias-smoke-exit
  took=$(($(date +%s) - start))
  check "$svc exits 0 on SIGTERM (${took}s)" test "$(cat /tmp/ozymandias-smoke-exit)" = 0 -a "$took" -le 10
done
# Restart the same containers (not `up`, which could rebuild or recreate).
"${COMPOSE[@]}" start >/dev/null 2>&1
for svc in ozyd agent; do
  for _ in $(seq 1 30); do healthy "$svc" && break; sleep 1; done
  check "$svc healthy again after restart" healthy "$svc"
done

# --- M1: statsd in, query out --------------------------------------------------
# Every send below runs *inside* a container on the ozymandias network, not from
# the Mac: Colima's ssh port forwarder drops UDP, so `nc` on the host would
# silently send into a void (docs/operations.md). busybox is the smallest image
# with an `nc` that speaks UDP.
#
# `-w1`, not `-w0`: busybox reads -w0 as "wait forever" (BSD nc reads it as
# "don't wait"), so -w0 hangs the container. -w1 costs a second per datagram,
# which is why sends are batched into as few containers as possible.
statsd() { # <line>... — all lines in ONE datagram (statsd is newline-framed)
  printf '%s\n' "$@" | docker run --rm -i --network ozymandias busybox nc -u -w1 agent 8125
}
statsd_burst() { # <datagrams> <lines each> <line> — separate datagrams, one container
  docker run --rm --network ozymandias -e D="$1" -e L="$2" -e LINE="$3" busybox sh -c '
    i=0
    while [ "$i" -lt "$D" ]; do
      j=0
      while [ "$j" -lt "$L" ]; do printf "%s\n" "$LINE"; j=$((j + 1)); done |
        nc -u -w1 agent 8125
      i=$((i + 1))
    done'
}
# query_sum <metric> — Σ since this run started, "null" if the metric is unknown.
# Scoped to the run, not to a fixed window: two smoke runs in a row would
# otherwise sum to 50 and fail. One interval of slack, because the bucket that
# holds the first sample starts before we do.
query_sum() {
  curl -fsS --max-time 5 "$OZY_URL/api/v1/query?metric=$1&agg=sum&from=$((M1_T0 - 10))&to=$(date +%s)" |
    python3 -c 'import json,sys
d = json.load(sys.stdin)
pts = [p[1] for s in d.get("series", []) for p in s["points"] if p[1] is not None]
print(int(sum(pts)) if pts else "null")'
}
# wait_sum <metric> <expected> — poll until the agent flushes (10s) and the
# intake stores it. Generous: a cold SQLite write on a loaded laptop is slow.
wait_sum() {
  for _ in $(seq 1 30); do
    [[ "$(query_sum "$1")" == "$2" ]] && return 0
    sleep 1
  done
  echo "  $1: Σ = $(query_sum "$1"), expected $2" >&2
  return 1
}

# Send everything first, then assert: one 10s flush covers all of it, so the
# whole M1 block costs one flush rather than one per check.
M1_T0=$(date +%s)
N=25
statsd_burst 5 5 'smoke.test:1|c|#source:smoke'          # 5 datagrams × 5 lines
statsd 'smoke.gauge:1|g' 'smoke.gauge:2|g' 'smoke.gauge:7|g'
# The protocol is the interface: no SDK, no library, just a shell script and nc.
check "examples/cron-script.sh runs"       docker run --rm --network ozymandias \
  -e OZY_AGENT_HOST=agent -e JOB=smoke \
  -v "$PWD/examples:/examples:ro" busybox sh /examples/cron-script.sh

check "statsd counter queries back (Σ=$N)" wait_sum smoke.test "$N"
# A gauge is last-write-wins within a flush, not a sum: three values, one point.
check "gauge keeps the last value"         wait_sum smoke.gauge 7
check "the cron script's counter landed"   wait_sum cron.job.runs 1
check "metric appears in the catalogue"    body_has "$OZY_URL/api/v1/metrics?prefix=smoke" '"smoke.test"'
check "its tag key is listed"              body_has "$OZY_URL/api/v1/tags?metric=smoke.test" '"source"'
check "its tag value is listed"            body_has "$OZY_URL/api/v1/tags/values?metric=smoke.test&key=source" '"smoke"'
check "the histogram became percentiles"   body_has "$OZY_URL/api/v1/metrics?prefix=cron.job.duration" '"cron.job.duration.95percentile"'
check "the agent counts what it parsed"    body_has "$AGENT_URL/debug/vars" 'ozy.agent.statsd.messages_received'

# --- M2: the real TSDB ---------------------------------------------------------
# The engine is meant to be invisible from out here — M1's checks above ran
# against it already. What is left to prove is that it is the engine actually
# running, that it reports what it refused, and that a block cut and a restart
# do not change an answer.
gauge_value() { # <metric> <tag> — an ozyd self-metric from /debug/vars
  curl -fsS --max-time 5 "$OZY_URL/debug/vars" |
    tr '{' '\n' | grep -F "\"$1\"" | grep -F "$2" | sed 's/.*"value"://; s/[^0-9.-].*//'
}

check "the tsdb is the store in use"       body_has "$OZY_URL/debug/vars" '"store:tsdb"'
check "it reports its head size"           body_has "$OZY_URL/debug/vars" 'ozy.tsdb.head.series'
check "it reports what it refused"         body_has "$OZY_URL/debug/vars" 'ozy.tsdb.ooo_rejected'
# Nothing above sends out-of-order data, so a non-zero count here means the
# store is dropping samples nobody asked it to drop.
check "no samples were dropped"            test "$(gauge_value ozy.tsdb.ooo_rejected store:tsdb)" = "0"
check "no series hit the cardinality cap"  test "$(gauge_value ozy.tsdb.series_limit_rejected store:tsdb)" = "0"
check "the store reports its disk use"     test "$(gauge_value ozy.tsdb.disk_bytes store:tsdb)" -gt 0

# The durability claim, end to end. The SIGTERM cycle near the top of this
# script happened before any of this data existed, so repeating a query after
# it proved nothing — which is what this check used to do. Kill ozyd
# *now*, with these samples in the head and durable only in the write-ahead
# log, and ask again on the other side. This is the only end-to-end exercise
# of replay there is, and replay is where every serious bug in M2 was.
cid=$("${COMPOSE[@]}" ps -q ozyd)
docker kill -s TERM "$cid" >/dev/null
docker wait "$cid" >/dev/null
"${COMPOSE[@]}" start ozyd >/dev/null 2>&1
for _ in $(seq 1 30); do healthy ozyd && break; sleep 1; done
check "ozyd healthy after the durability restart" healthy ozyd
check "the counter survived a real restart"  wait_sum smoke.test "$N"
check "the gauge survived a real restart"    wait_sum smoke.gauge 7

echo "smoke: $pass checks passed"
