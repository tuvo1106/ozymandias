#!/usr/bin/env python3
"""Show what a recovery threshold buys: two monitors on one oscillating gauge.

Creates two monitors on a gauge of this run's own, identical except that one has
`critical_recovery: 10` and the other leaves recovery at the threshold (20).
Then drives the gauge: 5 (both OK), 25 (both ALERT), then swings between 22 and
14 across the threshold for a few minutes, then 5 again.

The monitor with a recovery level stays ALERT through the swings, because 14 is
not below 10, and notifies twice in all: the ALERT and one recovery. The one
without flaps on every swing. The script counts each monitor's deliveries from
the events API (`notify_ok`), prints both counts, deletes both monitors, and
exits non-zero unless the monitor with a recovery level notified exactly twice
and the one without notified more than that.

    python3 scripts/demo/hysteresis.py                  # ozyd on localhost:9400
    OZY_URL=http://localhost:9400 python3 scripts/demo/hysteresis.py --swings 8

Points go straight to ozyd's intake (`/v1/series`, as `scripts/smoke.sh` does),
so no agent and no UDP are involved. Notifications go to the `log` notifier.
Standard library only; takes about `swings x 40s + 2 minutes` (about 6 minutes).
"""
import argparse
import json
import os
import secrets
import sys
import time
import urllib.request

METRIC = "demo.hysteresis"


def stamp(msg: str) -> None:
    print(f"{time.strftime('%H:%M:%S')} {msg}", flush=True)


class Ozy:
    def __init__(self, base: str):
        self.base = base.rstrip("/")

    def call(self, method: str, path: str, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=10) as r:
            raw = r.read()
            return json.loads(raw) if raw else None

    def push(self, run: str, value: float) -> None:
        self.call("POST", "/v1/series", {"series": [{"metric": METRIC, "type": "gauge", "interval": 0,
                                                     "tags": [f"run:{run}"], "points": [[int(time.time()), value]]}]})

    def status(self, mid: int) -> str:
        return self.call("GET", f"/api/v1/monitors/{mid}")["status"]

    def events(self, mid: int, kind: str) -> list:
        events = self.call("GET", f"/api/v1/events?monitor_id={mid}&kind={kind}")["events"]
        return sorted(events, key=lambda e: e["id"])  # ids are in commit order; ts strings do not sort


def monitor(run: str, name: str, thresholds: dict) -> dict:
    return {
        "uid": f"demo-hysteresis-{name}-{run}",
        "name": f"demo hysteresis ({name}) {run}",
        "type": "metric",
        "query": f"last(last_1m):avg:{METRIC}{{run:{run}}} > 20",
        "options": {"thresholds": thresholds, "evaluation_interval_s": 10, "evaluation_delay_s": 10},
        "message": "{{.Status}} at {{.Value}} @log",
        "tags": ["app:demo"],
    }


def hold(ozy: Ozy, run: str, value: float, seconds: float) -> None:
    """Pushes value every 5s for seconds."""
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        ozy.push(run, value)
        time.sleep(5)


def until(ozy: Ozy, run: str, value: float, mid: int, want: str, limit: float = 120) -> None:
    end = time.monotonic() + limit
    while time.monotonic() < end:
        ozy.push(run, value)
        if ozy.status(mid) == want:
            return
        time.sleep(5)
    sys.exit(f"monitor {mid} did not reach {want} within {limit:.0f}s")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--url", default=os.environ.get("OZY_URL", "http://localhost:9400"))
    ap.add_argument("--swings", type=int, default=6, help="times the gauge crosses the threshold and back")
    args = ap.parse_args()

    ozy = Ozy(args.url)
    run = secrets.token_hex(3)
    created = []
    try:
        banded = ozy.call("POST", "/api/v1/monitors", monitor(run, "banded", {"critical": 20, "critical_recovery": 10}))["id"]
        created.append(banded)
        naive = ozy.call("POST", "/api/v1/monitors", monitor(run, "naive", {"critical": 20}))["id"]
        created.append(naive)
        stamp(f"run {run}: monitor {banded} recovers below 10, monitor {naive} below 20")
        until(ozy, run, 5, banded, "OK")
        stamp("both OK at 5")
        until(ozy, run, 25, banded, "ALERT")
        stamp("ALERT at 25")
        for i in range(args.swings):
            hold(ozy, run, 14, 20)
            hold(ozy, run, 22, 20)
            stamp(f"swing {i + 1}/{args.swings}: 14 then 22; banded={ozy.status(banded)} naive={ozy.status(naive)}")
        until(ozy, run, 5, banded, "OK")
        hold(ozy, run, 5, 30)  # let both settle and every delivery land
        b, n = ozy.events(banded, "notify_ok"), ozy.events(naive, "notify_ok")
        for name, mid, sent in (("with a recovery level:    ", banded, b), ("recovery at the threshold:", naive, n)):
            path = " ".join(e["to"] for e in ozy.events(mid, "transition"))
            stamp(f"{name} {len(sent)} notifications; transitions: {path}")
    finally:
        for mid in created:
            try:
                ozy.call("DELETE", f"/api/v1/monitors/{mid}")
            except Exception as e:  # report, and keep going: the original error matters more
                print(f"could not delete monitor {mid}: {e}; delete it by hand", file=sys.stderr)
    # The demo shows something only if both hold: the banded monitor notified once each way, and
    # the naive one, on the same data, flapped.
    if len(b) != 2:
        sys.exit(f"expected exactly 2 notifications with a recovery level, got {len(b)}")
    if len(n) <= 2:
        sys.exit(f"the monitor without a recovery level notified only {len(n)} times: the swings did not cross the threshold, so this run shows nothing")


if __name__ == "__main__":
    main()
