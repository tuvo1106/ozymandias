# ADR-0017: A batch reports per query, not per series

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

[`docs/plan/M3-query-dashboards.md`](../plan/M3-query-dashboards.md) §1 says the
query response carries a per-series `"expr_index"`, and §2 adds
`POST /api/v1/query/batch {queries:[…], from, to}` to evaluate a whole dashboard
in one request. `expr_index` was deliberately not built in M3 §1 (PR #22): on
`/api/v1/query` one request evaluates one expression, so the field would have
been `0` on every line. It was deferred to the batch endpoint, where the plan's
shape implies one flat `series` array whose lines are told apart by which query
produced them.

Building the endpoint made the flat shape's real problem visible, and it is not
about ergonomics. A dashboard is a set of *independent* questions. One widget
asking for a percentile of a gauge, or naming a metric that was renamed last
week, is a failure that belongs to that widget. A flat array of series has
nowhere to put it: the request either succeeds — and silently omits the lines
that failed, so a broken widget looks like an empty one — or it fails, and one
typo blanks the other eleven charts.

That is the same failure mode as the dashboard list before PR #24, where one
unreadable row made `GET /api/v1/dashboards` a 500 for every dashboard.

## Decision

**The batch response is an array of per-query results, and `expr_index` does not
exist.**

```json
{"status":"ok","from":…,"to":…,"interval":…,
 "results":[
   {"index":0,"status":"ok","query":"sum:…","series":[…],"warnings":[]},
   {"index":1,"status":"error","query":"p95:temp{*}","series":[],"warnings":[],
    "code":400,"error":"temp is a gauge, so p95 is not a number it has: …"}]}
```

- `index` is the query's position in the request, carried explicitly so a result
  survives being fanned out to the widget that asked for it.
- `code` is the status the same query would have been answered with on
  `/api/v1/query` — 400 for the query's fault, 503 for out of time or not
  answerable here, 500 for ours — so a client has one rule for both endpoints.
  A 500's message is replaced and the real one logged, exactly as on
  `/api/v1/query`; the 200 around it changes nothing about that.
- `series` and `warnings` are always present, empty rather than absent.
- The window and the interval belong to the batch, reported once.

The HTTP status describes the *request*: 400 for a body that is not a batch, no
queries, or more than `eval.MaxQueriesPerBatch` of them. Otherwise 200, even when
every query in it failed.

`expr_index` is dropped rather than added to each series: `results[i]` already
answers "which query produced this", and one fact in two places is one fact that
can disagree with itself.

## Alternatives considered

| Option | Why not |
|---|---|
| The plan's shape: one flat `series` array with `expr_index` per line | Cannot express a per-query failure. A broken widget is then indistinguishable from an empty one, or fails the whole dashboard. |
| Flat array *and* a separate `errors` array keyed by index | Two arrays to join on the client, and the join is the thing `results[]` already is. |
| Per-query results **and** `expr_index` on each series, for clients that flatten | Redundant, and redundancy here is a claim that must stay true: `results[i].series[*].expr_index == i` is one more invariant to test and to break. |
| Fail the request on the first bad query (400 for the batch) | One typo in one widget blanks the dashboard. This is the behaviour the endpoint exists to avoid. |
| Return 207 Multi-Status when some queries failed | Nothing outside WebDAV knows it, clients and proxies treat it as an unknown 2xx anyway, and it says nothing a per-result `status` does not. |

## Consequences

- `/api/v1/query` keeps its flat, single-result shape and gains nothing; the two
  endpoints have different response shapes on purpose, because they answer
  different numbers of questions.
- A client renders a dashboard by mapping `results[i]` onto the widget that
  supplied query *i*, and shows `error` in the widget that failed.
- `expr_index` never appears. §1 and §2 of the plan are amended in this PR.
- If a later milestone wants queries evaluated over *different* windows in one
  request, this shape extends — a result can carry its own `from`/`to` — but the
  sharing that justifies the endpoint (ADR-0018) would stop applying.
