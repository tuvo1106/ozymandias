#!/usr/bin/env bash
# Drill: kill -9 ozyd during sustained ingest, restart, repeat.
# Expected (M7 §6): zero acknowledged-data loss and no duplicates visible in queries.
#
#   CHAOS_KILLS   how many kills (default 5; the acceptance run is 50)
#   CHAOS_MODE    queued (default) or direct
#   CHAOS_INGEST_RATE   ingest rounds per second (default 100: each is one series and one log).
#                       Not CHAOS_RATE, which is udp-flood's messages per second: all.sh long sets that to
#                       500000, and a sender that fast outruns the clock, so intake refuses its points.
source "$(dirname "$0")/lib.sh"

KILLS=${CHAOS_KILLS:-5}
RATE=${CHAOS_INGEST_RATE:-100}
ACKS=$WORK/acks.txt

start_ozyd
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"
ok "ozyd up (${CHAOS_MODE:-queued} mode, temp data dir)"

# One ingest process for the whole drill: it outlives every kill and records, for
# every request, whether ozyd promised it (202) or its fate is unknown.
DURATION=$((KILLS * 4 + 20))
in_background "$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate "$RATE" -duration "${DURATION}s"
INGEST=$LAST_BG

i=0
while [[ $i -lt $KILLS ]]; do
  # Random 1-3 s of ingest between kills, so kills land at different points: between
  # accept and fsync, between apply and commit, mid-rollover.
  sleep "$((1 + RANDOM % 3))"
  kill9_ozyd
  start_ozyd
  wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not come back after kill $((i + 1))"
  i=$((i + 1))
done
ok "$KILLS kill -9s survived, each followed by a restart that became ready"

wait "$INGEST" || fail "the ingest process failed"
grep -q "^l .* a " "$ACKS" || fail "nothing was acknowledged: the drill proved nothing"

eventually 90 "the queues never drained (lag_records > 0)" queues_drained
"$BIN/loadgen" -scenario verify -ozyd "$OZYD_URL" -acks "$ACKS" -settle 90s || fail "acknowledged data was lost or duplicated"
ok "every acknowledged log and point is stored, none twice"
