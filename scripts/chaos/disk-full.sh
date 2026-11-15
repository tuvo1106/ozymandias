#!/usr/bin/env bash
# Drill: the data volume fills up under ingest, then space is freed.
# Expected (M7 §6): intake refuses (no acknowledgement it cannot keep), ozyd does not
# panic or corrupt anything, and everything acknowledged is stored after recovery.
#
# Needs a small filesystem of its own. macOS: a 16 MB disk image (no root needed).
# Linux: a tmpfs, which needs root (sudo); the drill skips, loudly, without it (CHAOS_NO_SKIP=1: fails).
source "$(dirname "$0")/lib.sh"

MNT=$WORK/vol
mkdir -p "$MNT"
# A drill that cannot run is not a drill that passed. Locally a skip is fine; CI sets
# CHAOS_NO_SKIP so that a runner without the privilege fails instead of going quiet.
skip() {
  echo "SKIP: $1"
  [[ -z ${CHAOS_NO_SKIP:-} ]] || { echo "CHAOS_NO_SKIP is set: a skipped drill is a failure" >&2; exit 1; }
  exit 0
}
case "$(uname)" in
  Darwin)
    # detach is defined before the mount that needs it, so an interrupted run still unmounts
    # (lib.sh's cleanup calls it before removing $WORK, which contains the mount point).
    detach() { hdiutil detach -quiet -force "$MNT" 2>/dev/null || true; }
    hdiutil create -quiet -size 16m -fs 'HFS+' -volname ozychaos "$WORK/vol.dmg" || fail "could not create a disk image"
    hdiutil attach -quiet -nobrowse -mountpoint "$MNT" "$WORK/vol.dmg" || fail "could not attach the disk image"
    ;;
  Linux)
    sudo -n true 2>/dev/null || skip "disk-full needs root to mount a tmpfs, and sudo needs a password here"
    detach() { sudo umount -l "$MNT" 2>/dev/null || true; }
    sudo mount -t tmpfs -o size=16m tmpfs "$MNT" || fail "could not mount a tmpfs"
    sudo chown "$(id -u)" "$MNT" || fail "could not take ownership of the tmpfs"
    ;;
  *) skip "disk-full is not supported on $(uname)" ;;
esac

ACKS=$WORK/acks.txt
DATA=$MNT/data
start_ozyd OZY_DATA_DIR="$DATA"
wait_ready "$OZYD_URL/readyz" || fail "ozyd never became ready on the small volume"
ok "ozyd up on a 16 MB volume"

"$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate 50 -duration 5s >/dev/null
BEFORE=$(grep -c " a " "$ACKS" || true)
[[ $BEFORE -gt 0 ]] || fail "nothing was acknowledged before the disk filled"

# Fill whatever is left, leaving the ozyd running. dd stops at ENOSPC, which is the point.
dd if=/dev/zero of="$MNT/ballast" bs=64k 2>/dev/null || true
ok "the volume is full ($(df -k "$MNT" | awk 'NR==2 {print $4}') KB free)"

"$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate 50 -duration 8s >"$WORK/full.txt" 2>&1 || true
kill -0 "$OZYD_PID" 2>/dev/null || fail "ozyd died when the disk filled"
grep -qiE "panic|fatal error" "$WORK/ozyd.log" && fail "ozyd panicked when the disk filled"
curl -fsS --max-time 3 "$OZYD_URL/healthz" >/dev/null || fail "ozyd stopped answering /healthz when the disk filled"
ok "ozyd stayed up, did not panic, and still answers /healthz"
UNKNOWN=$(grep -c " u " "$ACKS" || true)
[[ $UNKNOWN -gt 0 ]] || fail "ozyd acknowledged everything sent to a full disk (it cannot have kept it)"
ok "intake refused while the disk was full ($UNKNOWN requests were not acknowledged)"

rm -f "$MNT/ballast"
# A store that hit a write error it cannot undo stops until restart (/readyz says so);
# one that undid it recovers by itself. Either is allowed; a restart is the supported
# repair, so do it, and then require that everything acknowledged is there.
kill9_ozyd
start_ozyd OZY_DATA_DIR="$DATA"
wait_ready "$OZYD_URL/readyz" 30 || fail "ozyd did not restart after space was freed"
"$BIN/loadgen" -scenario ack-ingest -ozyd "$OZYD_URL" -acks "$ACKS" -rate 50 -duration 3s >/dev/null
AFTER=$(grep -c " a " "$ACKS" || true)
[[ $AFTER -gt $BEFORE ]] || fail "after space was freed nothing was acknowledged again ($BEFORE -> $AFTER)"
ok "intake accepts again after space was freed"
eventually 60 "the queues never drained (lag_records > 0)" queues_drained
"$BIN/loadgen" -scenario verify -ozyd "$OZYD_URL" -acks "$ACKS" -settle 60s || fail "acknowledged data was lost or duplicated across the full disk"
ok "everything acknowledged, before, and after, is stored, none twice"
