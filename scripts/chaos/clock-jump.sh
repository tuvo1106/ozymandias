#!/usr/bin/env bash
# Drill: a sender whose clock jumps an hour forward and an hour back.
# Expected (M7 §6): future and ancient points are rejected and counted; the points
# around them are stored; nothing is corrupted, including across a restart.
#
# The jump is made by sending the timestamps a skewed host would send: a point from
# 1 h ahead and one from 1 h behind, between points with the right time. Skewing a
# real agent's clock needs libfaketime or root and proves nothing more about ozyd.
source "$(dirname "$0")/lib.sh"

start_ozyd
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"

NOW=$(date +%s)
send() { # <offset-seconds> <name>
  curl -fsS -X POST "$OZYD_URL/v1/series" -d "{\"series\":[{\"metric\":\"$2\",\"type\":\"gauge\",\"interval\":0,\"tags\":[],\"points\":[[$((NOW + $1)),1]]}]}" >/dev/null
}

# Each good point is a second apart; the bad ones sit between them.
send 0 clock.good
send 3600 clock.future      # +1 h: refused at intake as too far ahead
send -3600 clock.good       # -1 h on a series that has a newer point: refused as out of order
send 1 clock.good
send -86400 clock.good      # a day behind
send 2 clock.good
send 31536000 clock.future  # a year ahead
send 3 clock.good
# One counter per cause, each exact. Summing them would double-count (the consumer's and the
# TSDB's counters both see an out-of-order point), and a total of 'at least 4' would let a
# regression that admits the future points pass.
FUTURE=2 # +1h and +1y: refused at intake as too far ahead
OLD=2    # -1h and -1d: refused by the store as older than the series' newest point
eventually 20 "the future points were not each refused and counted at intake (series_rejected=$(metric "$OZYD_URL" ozy.intake.series_rejected), want $FUTURE)" \
  '[[ $(metric "$OZYD_URL" ozy.intake.series_rejected) = $FUTURE ]]'
eventually 20 "the out-of-order points were not each refused and counted by the store (ooo_rejected=$(metric "$OZYD_URL" ozy.tsdb.ooo_rejected), want $OLD)" \
  '[[ $(metric "$OZYD_URL" ozy.tsdb.ooo_rejected) = $OLD ]]'
ok "each bad point was refused and counted: $FUTURE at intake (too far ahead), $OLD by the store (out of order)"

eventually 20 "the points around the bad ones were not stored (clock.good=$(stored_sum clock.good 120 1))" '[[ $(stored_sum clock.good 120 1) = 4 ]]'
ok "the 4 points with the right time are stored; the ones with the wrong time are not"
[[ $(stored_sum clock.future 120 1) = 0 ]] || fail "a point from the future was stored"
ok "nothing from the future was stored"

# No corruption: the log replays, and the answer is the same.
kill9_ozyd
start_ozyd
wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not restart after the clock-jump points"
eventually 20 "after a hard kill and restart the stored points changed (clock.good=$(stored_sum clock.good 120 1))" '[[ $(stored_sum clock.good 120 1) = 4 ]]'
ok "after kill -9 and a restart the same 4 points are there"
