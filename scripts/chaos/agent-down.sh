#!/usr/bin/env bash
# Drill: the agent dies (kill -9) while an app keeps running, then restarts.
# Expected (M7 §6): the app is unaffected (a UDP send to a dead port does not fail),
# and the log lines written meanwhile are caught up from the file offsets on restart,
# with none repeated and none skipped.
source "$(dirname "$0")/lib.sh"

mkdir -p "$WORK/agent.d" "$WORK/app"
LOG=$WORK/app/app.log
cat >"$WORK/agent.d/logs.yaml" <<YAML
logs:
  enabled: true
  registry_path: $WORK/registry.json
  scan_interval: 1s
  poll_interval: 200ms
  sources:
    - type: file
      path: $WORK/app/*.log
      service: chaos-app
      source: plain
      start_position: beginning
YAML

start_ozyd
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"
start_agent OZY_AGENT_CONFD_PATH="$WORK/agent.d"
wait_ready "$AGENT_URL/healthz" || fail "the agent never became healthy"

# stored_lines: "<total> <distinct>" of the app's log messages in ozyd.
stored_lines() {
  local now; now=$(date +%s)
  curl -sS --max-time 30 "$OZYD_URL/api/v1/logs?q=service:chaos-app&from=$(( (now - 3600) * 1000 ))&to=$(( (now + 60) * 1000 ))&limit=10000" |
    python3 -c '
import json, sys
d = json.load(sys.stdin)
m = [l["message"] for l in d.get("logs", [])]
print(len(m), len(set(m)))'
}

i=1; while [[ $i -le 100 ]]; do echo "line $i"; i=$((i + 1)); done >"$LOG"
eventually 30 "the first 100 lines never arrived" '[[ $(stored_lines) = "100 100" ]]'
ok "100 lines written while the agent ran were stored"

kill -KILL "$AGENT_PID"; wait "$AGENT_PID" 2>/dev/null || true; AGENT_PID=""
i=101; while [[ $i -le 200 ]]; do echo "line $i"; i=$((i + 1)); done >>"$LOG"
# The app's metrics path with the agent gone: a UDP datagram to a closed port must not
# fail or block the sender (it would be the app's request path in a real deployment).
python3 - "$AGENT_UDP_PORT" <<'PY' || fail "sending statsd to a dead agent failed or blocked the app"
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
t = time.time()
for _ in range(1000):
    s.sendto(b"app.requests:1|c", ("127.0.0.1", int(sys.argv[1])))
assert time.time() - t < 2, "1000 sends took %.1fs" % (time.time() - t)
PY
ok "the app was unaffected while the agent was down (1000 statsd sends, no error, no stall)"
sleep 3
[[ $(stored_lines) = "100 100" ]] || fail "lines reached ozyd while the agent was dead: $(stored_lines)"

start_agent OZY_AGENT_CONFD_PATH="$WORK/agent.d"
wait_ready "$AGENT_URL/healthz" || fail "the agent did not restart"
eventually 40 "the lines written while the agent was down were not caught up (stored: $(stored_lines))" '[[ $(stored_lines) = "200 200" ]]'
ok "200 distinct lines stored after the restart: the 100 missed were caught up, none repeated"
