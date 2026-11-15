#!/usr/bin/env bash
# Drill: ozyd down while the agent keeps receiving; then ozyd comes back.
# Expected (M7 §6): agent memory bounded; the disk buffer grows, then drains;
# nothing the agent parsed is lost, nothing is counted twice.
#
#   CHAOS_DOWN   seconds ozyd stays down (default 20; the acceptance run is 900)
source "$(dirname "$0")/lib.sh"

DOWN=${CHAOS_DOWN:-20}
start_ozyd
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"
# A small memory queue and a 1 s flush make the agent spill to disk within seconds,
# which is the behaviour under test; the shipped defaults would take hours to fill.
start_agent OZY_AGENT_FORWARDER_BUFFER_PATH="$WORK/buffer" OZY_AGENT_FORWARDER_MAX_QUEUE_BYTES=4096 \
  OZY_AGENT_AGGREGATOR_FLUSH_INTERVAL=1s
wait_ready "$AGENT_URL/healthz" || fail "the agent never became healthy"
ok "ozyd and agent up"

flood() { "$BIN/loadgen" -scenario statsd-flood -addr "127.0.0.1:$AGENT_UDP_PORT" -agent "$AGENT_URL" -rate 20000 -duration "$1" -contexts 500; }

flood 3s >/dev/null
RSS0=$(rss_mb "$AGENT_PID")

kill9_ozyd
ok "ozyd killed"
flood "${DOWN}s" >"$WORK/flood.txt"
SPOOL=$(metric "$AGENT_URL" ozy.agent.forwarder.spool_bytes)
RSS1=$(rss_mb "$AGENT_PID")
[[ $SPOOL -gt 0 ]] || fail "ozyd was down for ${DOWN}s and the agent spooled nothing (spool_bytes=$SPOOL): the disk buffer was never exercised"
ok "the agent spooled $SPOOL bytes to disk while ozyd was down"
[[ $((RSS1 - RSS0)) -lt 200 ]] || fail "agent RSS grew from ${RSS0}MB to ${RSS1}MB while ozyd was down"
ok "agent memory stayed bounded (${RSS0}MB -> ${RSS1}MB)"

start_ozyd
wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not come back"
eventually 120 "the agent's buffer never drained" '[[ $(metric "$AGENT_URL" ozy.agent.forwarder.spool_payloads) = 0 ]]' 
ok "the buffer drained after ozyd returned"

# What the agent parsed is what must be stored: every message is a count of 1, so the
# stored sum is the number of messages the agent received, whatever the UDP lost
# before it. (Equal, not at least: a duplicate would be visible as an excess.)
sleep 5
RECEIVED=$(metric "$AGENT_URL" ozy.agent.statsd.messages_received)
eventually 60 "ozyd never stored all $RECEIVED parsed messages" '[[ $(stored_sum loadgen.flood) -ge $RECEIVED ]]' 
STORED=$(stored_sum loadgen.flood)
[[ $STORED -eq $RECEIVED ]] || fail "the agent parsed $RECEIVED messages but ozyd stored $STORED: $((STORED - RECEIVED)) too many (duplicates)"
ok "every one of the $RECEIVED parsed messages is stored, none twice"
