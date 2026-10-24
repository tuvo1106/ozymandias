#!/usr/bin/env python3
"""Flood app-python's judge queue, for the M6 backlog-monitor demo.

Registers a few throwaway accounts in the app (a local dev stack: `example.com`
addresses, random passwords that are never printed), verifies them through the
app's own CLI in its api container, and has each submit a slow program to one
problem. The program sleeps before the judge's per-case timer starts, so each
run holds a worker slot for that long.

The app allows each user one submission in flight, so the queue is about as
deep as the number of users: the default 30 users read 31-33 on 2026-10-23
(docs/notes/M6.md), past the judge-backlog monitor's 20. That is a measurement,
not a bound: what arq counts as queued is arq's business. Each user
resubmits as soon as its previous submission is judged, for `--duration`
seconds; then the queue drains and the monitor should recover by itself.

Registering is the slow part: register and login are both auth calls, limited
to 10 a minute per IP, so users are made 5 a minute (30 users take 6 minutes).
Every token is fetched up front and the app's access tokens last 15 minutes, so
registration plus `--duration` must fit in that: the script refuses settings
that do not, and stops saying so if a token expires anyway.

    python3 scripts/demo/flood-app-python.py                    # 30 users for 5 minutes
    python3 scripts/demo/flood-app-python.py --users 10 --duration 120
    BASE_URL=http://localhost:8080 API_CONTAINER=shikomi-api-1 python3 scripts/demo/flood-app-python.py

It prints the time of every step, so the notes can measure "flood started" to
"monitor alerted". Standard library only; needs `docker` for the verify step.
"""
import argparse
import json
import os
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request


# The app's access-token lifetime (its jwt_access_ttl_seconds default).
TOKEN_LIFETIME_S = 900


def stamp(msg: str) -> None:
    print(f"{time.strftime('%H:%M:%S')} {msg}", flush=True)


class App:
    def __init__(self, base: str):
        self.api = base.rstrip("/") + "/api/v1"

    def call(self, method: str, path: str, body=None, token=None):
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.api + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=15) as r:
                return r.status, json.loads(r.read() or b"null")
        except urllib.error.HTTPError as e:
            return e.code, None
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            # A flood slows the app down on purpose; a slow answer is not a reason to stop.
            return 0, None


def make_user(app: App, container: str) -> str:
    """Registers, verifies and logs in one throwaway user; returns its token."""
    email = f"flood-{secrets.token_hex(4)}@example.com"
    password = secrets.token_urlsafe(18)
    status, _ = app.call("POST", "/auth/register", {"email": email, "username": "flood_" + secrets.token_hex(3), "password": password})
    if status != 202:
        sys.exit(f"register answered {status} (the auth rate limit is per IP per minute; wait and retry)")
    # The judge endpoints need a verified email, and there is no inbox here.
    subprocess.run(["docker", "exec", container, "sh", "-c", f"cd /app && python -m app.cli verify-email {email}"],
                   check=True, capture_output=True)
    status, body = app.call("POST", "/auth/login", {"email": email, "password": password})
    if status != 200:
        sys.exit(f"login answered {status}")
    return body["access_token"]


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", default=os.environ.get("BASE_URL", "http://localhost:8080"))
    ap.add_argument("--container", default=os.environ.get("API_CONTAINER", "shikomi-api-1"))
    ap.add_argument("--problem", default="design-vending-machine")
    ap.add_argument("--users", type=int, default=30)
    ap.add_argument("--duration", type=float, default=300, help="seconds to keep every user's submission queued")
    ap.add_argument("--sleep", type=float, default=8, help="seconds each submission sleeps before running")
    args = ap.parse_args()
    registering = (args.users - 1) // 5 * 61
    if registering + args.duration > TOKEN_LIFETIME_S - 60:
        sys.exit(f"{args.users} users take about {registering}s to register; with --duration {args.duration:.0f} "
                 f"the first tokens expire mid-flood (they last {TOKEN_LIFETIME_S}s). Use fewer users or a shorter duration.")

    app = App(args.base)
    stamp(f"registering {args.users} users, 5 a minute (the auth rate limit)")
    tokens = []
    for i in range(args.users):
        if i and i % 5 == 0:
            time.sleep(61)
        tokens.append(make_user(app, args.container))
    status, problem = app.call("GET", f"/problems/{args.problem}", token=tokens[0])
    if status != 200:
        sys.exit(f"problem {args.problem}: {status}")
    starter = next(v["starter_code"] for v in problem["languages"] if v["language"] == "python")
    code = f"import time\ntime.sleep({args.sleep})\n\n{starter}"

    stamp(f"flood started: {args.users} users, each keeping one submission queued, each sleeping {args.sleep}s")
    pending: dict[str, str | None] = {t: None for t in tokens}  # token -> its submission in flight
    resting: dict[str, float] = {}  # token -> when it may POST again, after a 429
    sent = refused = failed = 0
    end = time.monotonic() + args.duration
    while time.monotonic() < end:
        for token, sid in pending.items():
            if resting.get(token, 0) > time.monotonic():
                continue
            if sid is not None:
                status, body = app.call("GET", f"/submissions/{sid}", token=token)
                if status == 401:
                    sys.exit("a token expired mid-flood; the numbers above are incomplete")
                if status == 0 or (status == 200 and body["status"] in ("pending", "running")):
                    continue  # still queued, or the app is too slow to say: ask again next round
            status, body = app.call("POST", "/submissions", {"problem_id": problem["id"], "code": code, "language": "python"}, token)
            if status in (200, 201, 202):
                pending[token] = body["id"]
                sent += 1
            elif status == 401:
                sys.exit("a token expired mid-flood; the numbers above are incomplete")
            elif status == 0:
                failed += 1  # no answer; the submission may or may not exist, so ask again
            elif status == 429:
                # In flight (a POST that timed out may have landed) or rate limited. The app counts
                # every POST against the per-user limit, so retrying each round would use it up.
                refused += 1
                resting[token] = time.monotonic() + 10
            else:
                pending[token] = None
                refused += 1
        time.sleep(2)
    stamp(f"flood stopped: {sent} submitted, {refused} refused, {failed} unanswered; the queue now drains")


if __name__ == "__main__":
    main()
