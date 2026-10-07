#!/usr/bin/env python3
"""Post synthetic traces to a running agent, for looking at the APM pages and for the UI e2e test.

Three services (a web api, a worker reached through a queue, a billing service), a few routes,
some slow requests, some failing, one trace with a queue wait between arq.enqueue and arq.job.
Everything is invented: no app, no real data. Ids are derived from the run label, so a second
run with the same label re-posts the same traces (the store is idempotent).

    python3 scripts/seed-traces.py                      # agent on localhost:8126, label "seed"
    OZY_AGENT=http://localhost:8126 python3 scripts/seed-traces.py --label e2e --count 40
"""
import argparse
import hashlib
import json
import os
import random
import time
import urllib.request


def hid(label: str, *parts, nbytes: int) -> str:
    h = hashlib.sha256((label + "|" + "|".join(map(str, parts))).encode()).hexdigest()[: nbytes * 2]
    return h if h.strip("0") else "1" * (nbytes * 2)


def span(trace, sid, parent, service, name, resource, typ, start, dur, err=0, meta=None, metrics=None):
    return {
        "trace_id": trace, "span_id": sid, "parent_id": parent, "service": service, "name": name,
        "resource": resource, "type": typ, "start": int(start), "duration": int(dur), "error": err,
        "meta": {"env": "dev", **(meta or {})}, "metrics": metrics or {},
    }


def build(label: str, i: int, now_us: int, rng: random.Random):
    """One trace as a list of chunks (one per process), like the SDKs send."""
    trace = hid(label, "t", i, nbytes=16)
    s = lambda n: hid(label, i, n, nbytes=8)  # noqa: E731
    start = now_us - rng.randint(0, 25 * 60) * 1_000_000 - 5_000_000
    route = rng.choice(["/api/items/:id", "/api/items", "/api/orders", "/api/search"])
    method = "POST" if route == "/api/orders" else "GET"
    failing = i % 9 == 0
    slow = i % 7 == 0
    dur = (900_000 if slow else rng.randint(8_000, 60_000))
    status = "503" if failing else "200"
    api = [
        span(trace, s(1), None, "shop-api", "http.request", f"{method} {route}", "web", start, dur, int(failing),
             {"http.method": method, "http.route": route, "http.status_code": status, "span.kind": "server"},
             {"_top_level": 1, "_sampling_priority": 1}),
        span(trace, s(2), s(1), "shop-api", "sqlite.query", "SELECT * FROM items WHERE id = ?", "db", start + 2_000,
             dur // 3, 0, {"db.system": "sqlite"}),
    ]
    if failing:
        api[0]["meta"].update({"error.type": "UpstreamError", "error.message": "billing unavailable"})
        api[1]["error"] = 0
    chunks = [api]
    if route == "/api/orders":
        enq = span(trace, s(3), s(1), "shop-api", "arq.enqueue", "charge_order", "queue", start + dur // 2, 1_500, 0,
                   {"span.kind": "producer"})
        api.append(enq)
        wait = 40_000 + rng.randint(0, 30_000)
        job_start = enq["start"] + enq["duration"] + wait
        job_dur = 120_000
        chunks.append([
            span(trace, s(4), s(3), "billing-worker", "arq.job", "charge_order", "worker", job_start, job_dur, int(failing),
                 {"span.kind": "consumer", "job.id": f"job-{i}"}, {"_top_level": 1, "queue.wait_ms": wait / 1000}),
            span(trace, s(5), s(4), "billing-worker", "postgres.query", "INSERT INTO charges (order_id) VALUES ($1)", "db",
                 job_start + 5_000, 8_000, 0, {"db.system": "postgresql"}),
        ])
    return chunks


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--label", default="seed")
    ap.add_argument("--count", type=int, default=60)
    ap.add_argument("--agent", default=os.environ.get("OZY_AGENT", "http://localhost:8126"))
    a = ap.parse_args()
    rng = random.Random(a.label)
    now_us = int(time.time() * 1e6)
    chunks = [c for i in range(a.count) for c in build(a.label, i, now_us, rng)]
    for k in range(0, len(chunks), 100):
        body = json.dumps({"tracer": {"lang": "seed", "version": "1"}, "traces": chunks[k : k + 100]}).encode()
        req = urllib.request.Request(a.agent + "/v1/traces", body, {"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=10) as r:
            out = json.load(r)
    print(f"posted {a.count} traces ({sum(len(c) for c in chunks)} spans) to {a.agent}; last response {out['accepted']} accepted, {out['rejected']} rejected")


if __name__ == "__main__":
    main()
