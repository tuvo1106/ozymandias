#!/usr/bin/env bash
# Drill: a UDP flood at the agent's statsd port.
# Expected (M7 §6): the agent stays up, loss is counted, and the other pipelines
# (traces here; logs are a file tail and never share the UDP socket) keep flowing.
#
#   CHAOS_RATE    messages per second (default 200000; the acceptance run is 500000)
#   CHAOS_FLOOD   seconds (default 15; the acceptance run is 60)
source "$(dirname "$0")/lib.sh"

RATE=${CHAOS_RATE:-200000}
FLOOD=${CHAOS_FLOOD:-15}
start_ozyd
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"
start_agent OZY_AGENT_AGGREGATOR_FLUSH_INTERVAL=1s
wait_ready "$AGENT_URL/healthz" || fail "the agent never became healthy"
ok "ozyd and agent up"

SPANS0=$(metric "$OZYD_URL" ozy.tracestore.spans_appended)
"$BIN/loadgen" -scenario statsd-flood -addr "127.0.0.1:$AGENT_UDP_PORT" -agent "$AGENT_URL" -rate "$RATE" -duration "${FLOOD}s" -contexts 500 >"$WORK/flood.txt" &
FLOODER=$!
BG_PIDS="$BG_PIDS $FLOODER"

# Traces go through the agent's HTTP port while the UDP socket is being hammered.
sleep 2
for label in a b c; do
  OZY_AGENT=$AGENT_URL python3 scripts/seed-traces.py --label "flood-$label" --count 20 >/dev/null || fail "the agent refused traces during the flood"
  sleep 1
done
kill -0 "$AGENT_PID" 2>/dev/null || fail "the agent died during the flood"
curl -fsS --max-time 3 "$AGENT_URL/healthz" >/dev/null || fail "the agent stopped answering /healthz during the flood"
ok "the agent was alive and answering during the flood"

wait "$FLOODER" || fail "the load generator failed"
cat "$WORK/flood.txt" | sed 's/^/    /'
RECEIVED=$(metric "$AGENT_URL" ozy.agent.statsd.messages_received)
[[ $RECEIVED -gt 0 ]] || fail "the agent parsed no messages at all"
ok "the agent parsed $RECEIVED messages; the rest is loss, reported above"

eventually 30 "no spans reached ozyd while the flood ran (spans_appended $SPANS0 -> $(metric "$OZYD_URL" ozy.tracestore.spans_appended))" \
  '[[ $(metric "$OZYD_URL" ozy.tracestore.spans_appended) -gt $SPANS0 ]]'
ok "traces kept flowing: spans_appended $SPANS0 -> $(metric "$OZYD_URL" ozy.tracestore.spans_appended)"

# It still works afterwards: a statsd message sent now is stored.
printf 'chaos.after:1|c' | python3 -c 'import socket,sys; socket.socket(socket.AF_INET, socket.SOCK_DGRAM).sendto(sys.stdin.buffer.read(), ("127.0.0.1", int(sys.argv[1])))' "$AGENT_UDP_PORT"
eventually 30 "the agent did not recover after the flood: a message sent afterwards never arrived" '[[ $(stored_sum chaos.after) -ge 1 ]]'
ok "the agent recovered: a message sent after the flood is stored"
