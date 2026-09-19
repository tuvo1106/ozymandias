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

echo "smoke: $pass checks passed"
