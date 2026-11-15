#!/usr/bin/env bash
# Drill: one sender mints series without bound (a tag that carries a user id, say).
# Expected (M7 §6): the per-metric series limit holds, other metrics are unaffected,
# and memory stays bounded.
#
# Per-key limits (a tenant's own cap) are not built yet: the limit under test is the
# global storage.max_series_per_metric. See docs/notes/M7.md.
#
#   CHAOS_SERIES   series the bomb tries to create (default 200000; the acceptance run is 1000000)
source "$(dirname "$0")/lib.sh"

SERIES=${CHAOS_SERIES:-200000}
start_ozyd
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"
RSS0=$(rss_mb "$OZYD_PID")

# A heartbeat on another metric, once a second, for the whole drill: it must never be refused.
cat >"$WORK/heartbeat.py" <<'PY'
import json, sys, time, urllib.request
url, dur = sys.argv[1], int(sys.argv[2])
sent = bad = 0
end = time.time() + dur
t = int(time.time())
while time.time() < end:
    t = max(t + 1, int(time.time()))
    body = json.dumps({"series": [{"metric": "chaos.heartbeat", "type": "gauge", "interval": 0, "tags": [], "points": [[t, 1]]}]}).encode()
    try:
        r = json.load(urllib.request.urlopen(urllib.request.Request(url + "/v1/series", body, {"Content-Type": "application/json"}), timeout=5))
        ok = r.get("accepted") == 1 and r.get("rejected") == 0
    except Exception:
        ok = False
    sent += 1
    bad += 0 if ok else 1
    time.sleep(1)
print(sent, bad)
PY
python3 "$WORK/heartbeat.py" "$OZYD_URL" $((SERIES / 20000 + 25)) >"$WORK/heartbeat.out" &
HB=$!
BG_PIDS="$BG_PIDS $HB"

python3 - "$OZYD_URL" "$SERIES" <<'PY' || fail "the bomb could not be sent"
import json, sys, time, urllib.request
url, n = sys.argv[1], int(sys.argv[2])
now = int(time.time())
accepted = rejected = 0
for lo in range(0, n, 1000):
    series = [{"metric": "chaos.bomb", "type": "gauge", "interval": 0, "tags": ["user:u%d" % i], "points": [[now, 1]]} for i in range(lo, min(lo + 1000, n))]
    body = json.dumps({"series": series}).encode()
    r = json.load(urllib.request.urlopen(urllib.request.Request(url + "/v1/series", body, {"Content-Type": "application/json"}), timeout=30))
    accepted += r["accepted"]; rejected += r["rejected"]
print("bomb: %d series accepted, %d rejected" % (accepted, rejected))
PY
HEAD=$(metric "$OZYD_URL" ozy.tsdb.head.series)
LIMITED=$(metric "$OZYD_URL" ozy.tsdb.series_limit_rejected)
# Queued mode applies after it acknowledges, so the limit shows up as the stores catch up.
eventually 60 "the series limit never engaged (series_limit_rejected=$(metric "$OZYD_URL" ozy.tsdb.series_limit_rejected), head=$(metric "$OZYD_URL" ozy.tsdb.head.series))" \
  '[[ $(metric "$OZYD_URL" ozy.tsdb.series_limit_rejected) -gt 0 ]]'
HEAD=$(metric "$OZYD_URL" ozy.tsdb.head.series)
# 10000 for the bomb's metric, plus the heartbeat and ozyd's own series.
[[ $HEAD -le 11000 ]] || fail "the head holds $HEAD series after a bomb of $SERIES: the per-metric limit did not hold"
ok "the series limit held: $HEAD series in the head after $SERIES were offered ($(metric "$OZYD_URL" ozy.tsdb.series_limit_rejected) refused)"

RSS1=$(rss_mb "$OZYD_PID")
[[ $((RSS1 - RSS0)) -lt 500 ]] || fail "ozyd's RSS grew from ${RSS0}MB to ${RSS1}MB"
ok "memory stayed bounded (${RSS0}MB -> ${RSS1}MB)"

wait "$HB" || true
read -r SENT BAD <"$WORK/heartbeat.out" || fail "the heartbeat produced no result"
[[ $SENT -gt 5 && $BAD -eq 0 ]] || fail "the heartbeat metric was refused $BAD of $SENT times while the bomb ran"
ok "another metric was unaffected: $SENT heartbeats, none refused"
eventually 30 "the heartbeats were not stored" '[[ $(stored_sum chaos.heartbeat 600 1) -ge $((SENT - 1)) ]]'
ok "and they are stored"
