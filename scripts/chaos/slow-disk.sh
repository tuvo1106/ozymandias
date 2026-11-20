#!/usr/bin/env bash
# Drill: the disk gets slow (every fsync takes a second or two), then fast again.
# Expected (M7 §6): backpressure engages and an ack never precedes durability. In queued mode that
# means, each shown below: an ack waits for the queue's fsync; slow stores let a backlog build while
# the queue keeps acknowledging; the backlog drains by itself when the disk recovers; reads are not
# held up; ozyd neither dies nor reports itself broken (a slow disk is not a failed one); and a kill -9
# in the middle of a backlog loses nothing that was acknowledged.
#
# Nothing outside the process can slow a disk here (dm-delay needs Linux and root; a Mac has no FUSE),
# so this builds its own ozyd with -tags faultinject, which sleeps before each write-ahead-log fsync
# for as long as a fault file says, per path (internal/tsdb/wal/faultinject.go). The ozyd that ships
# has no such code. Pebble's fsyncs (traces, sketches) are not slowed, and the queue's size cap, which
# turns a full queue into 429s, is not reached: it must be at least 64 MiB, which this drill's ingest
# rate would take hours to fill (the cap has its own tests).
#
#   CHAOS_SLOW_FRONT_MS   fsync delay on the queue, which an ack waits for (default 1000)
#   CHAOS_SLOW_STORE_MS   fsync delay on the stores' logs, which the consumers wait for (default 2000)
source "$(dirname "$0")/lib.sh"

if [[ ${CHAOS_MODE:-queued} != queued ]]; then
  echo "SKIP: slow-disk is a queued-mode drill"
  [[ -z ${CHAOS_NO_SKIP:-} ]] || { echo "CHAOS_NO_SKIP is set: a skipped drill is a failure" >&2; exit 1; }
  exit 0
fi
command -v go >/dev/null || fail "go is needed to build ozyd with the fault hook"

FRONT_MS=${CHAOS_SLOW_FRONT_MS:-1000}
STORE_MS=${CHAOS_SLOW_STORE_MS:-2000}
# Below 100 ms an fsync's own cost is in the noise of the comparison this drill makes.
[[ $FRONT_MS -ge 100 && $STORE_MS -ge 100 ]] || fail "CHAOS_SLOW_FRONT_MS and CHAOS_SLOW_STORE_MS must be at least 100"
FAULTS=$WORK/faults
ACKS=$WORK/acks.txt

go build -tags faultinject -o "$WORK/ozyd-fault" ./cmd/ozyd || fail "could not build ozyd with the fault hook"
OZYD_BIN=$WORK/ozyd-fault
slow_queue() { echo "/queue/ ${FRONT_MS}ms" >"$FAULTS"; }
slow_stores() { printf '/tsdb/wal/ %sms\n/logs/wal/ %sms\n' "$STORE_MS" "$STORE_MS" >"$FAULTS"; }
fast_disk() { rm -f "$FAULTS"; }
# ack_p50_ms: the median time to an acknowledgement over five single-point requests (one outlier
# cannot move it). It fails, and says so, when loadgen does.
ack_p50_ms() {
  local out
  out=$("$BIN/loadgen" -scenario intake-latency -ozyd "$OZYD_URL" -n 5) || return 1
  echo "$out" | awk '/acknowledged/ {printf "%d\n", $4}'
}
# no_panic <when>: the log holds no Go panic or fatal error. Matched at the start of a line, where the
# runtime writes them, so that a log message that mentions the word does not fail a drill.
no_panic() {
  ! grep -qE '^(panic:|fatal error:)' "$WORK/ozyd.log" || fail "ozyd panicked $1"
}
acked() { grep -c " a " "$ACKS" || true; }
unacked() { grep -c " u " "$ACKS" || true; }

start_ozyd OZY_FAULT_FSYNC_FILE="$FAULTS"
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready"
ok "ozyd up (fault-hook build, queued mode, the disk is fast)"

"$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate 50 -duration 4s >/dev/null
[[ $(acked) -gt 0 ]] || fail "nothing was acknowledged on a fast disk"
eventually 30 "the queues never drained on a fast disk" queues_drained
ok "on a fast disk everything is acknowledged and the queues are empty"

# 1. The front door: an ack is the queue's fsync.
FAST=$(ack_p50_ms) || fail "loadgen could not measure acknowledgements on a fast disk"
slow_queue
SLOW=$(ack_p50_ms) || { fast_disk; fail "loadgen could not measure acknowledgements with a slow queue fsync"; }
fast_disk
[[ $SLOW -ge $((FRONT_MS * 9 / 10)) ]] || fail "with a ${FRONT_MS} ms queue fsync an ack took ${SLOW} ms: acknowledged before it was durable?"
[[ $FAST -lt $((FRONT_MS / 2)) ]] || fail "a fast disk took ${FAST} ms to acknowledge: the comparison means nothing"
ok "an ack waits for the fsync: p50 ${SLOW} ms with a ${FRONT_MS} ms fsync, ${FAST} ms without"

# 2. Slow stores, fast queue: the queue keeps acknowledging and a backlog builds behind the consumers.
slow_stores
A0=$(acked); U0=$(unacked)
"$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate 100 -duration 10s >/dev/null
A=$(( $(acked) - A0 )); U=$(( $(unacked) - U0 ))
LAG=$(metric "$OZYD_URL" ozy.queue.lag_records)
kill -0 "$OZYD_PID" 2>/dev/null || fail "ozyd died on a slow disk"
no_panic "on a slow disk"
[[ $A -ge $(( (A + U) * 8 / 10 )) ]] || fail "only $A of $((A + U)) requests were acknowledged while the stores were slow: the queue should not wait for them"
[[ $LAG -gt 50 ]] || fail "no backlog built behind the slow stores (lag ${LAG} records): the stores were not slow, or the drill proved nothing"
ok "the stores were slow and the queue kept acknowledging: $A of $((A + U)) acked, $LAG records waiting"
curl -fsS --max-time 3 "$OZYD_URL/healthz" >/dev/null || fail "/healthz did not answer on a slow disk"
curl -fsS --max-time 3 "$OZYD_URL/readyz" >/dev/null || fail "/readyz reported not ready on a slow disk: a slow disk is not a failed one"
NOW=$(date +%s)
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$OZYD_URL/api/v1/query?metric=chaos.points&agg=sum&from=$((NOW - 300))&to=$((NOW + 60))&interval=60") || true
[[ $code = 200 ]] || fail "a read took more than 3 s or failed (HTTP $code) while the stores were slow"
ok "ozyd stayed up and ready, and a query was still answered, with the stores slow"

# 3. The disk recovers: the backlog drains with no restart.
fast_disk
START=$(date +%s)
eventually 90 "the backlog never drained after the disk sped up" queues_drained
ok "the backlog ($LAG records) drained by itself in $(( $(date +%s) - START )) s once the disk was fast"
no_panic "while the backlog drained"

# 4. Slow again, and a kill -9 in the middle of a backlog: what was acknowledged must still arrive.
slow_stores
"$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate 100 -duration 6s >/dev/null
LAG=$(metric "$OZYD_URL" ozy.queue.lag_records)
[[ $LAG -gt 0 ]] || fail "no backlog at the moment of the kill: the drill proved nothing"
kill9_ozyd
fast_disk
start_ozyd OZY_FAULT_FSYNC_FILE="$FAULTS"
wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not come back after a kill during a backlog"
ok "killed with $LAG records acknowledged and not yet stored; ozyd came back"
eventually 90 "the queues never drained after the restart" queues_drained
"$BIN/loadgen" -scenario verify -ozyd "$OZYD_URL" -acks "$ACKS" -settle 60s || fail "acknowledged data was lost or duplicated across the slow disk"
ok "everything acknowledged, through both slow phases and the kill, is stored, none twice"
no_panic "after the restart"
