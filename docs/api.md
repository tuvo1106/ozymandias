# HTTP API reference

Every HTTP route ozyd and the agent serve. `scripts/check-docs.sh` fails
CI if a route registered in code is missing here.

The telemetry intake endpoints (`/v1/*`) are specified byte-for-byte in
[wire-protocol.md](wire-protocol.md); their summary is below. This page
covers everything else.

## Common to both binaries

### `GET /healthz`

Liveness: answers `200` whenever the process is serving. It is not a
readiness check. M7 adds `/readyz` for "stores open, WAL replayed".

```console
$ curl -s localhost:9400/healthz
{"component":"ozyd","status":"ok","uptime_seconds":42,"version":"v0.0.0-3-gabc123"}
```

| Field | Meaning |
|---|---|
| `status` | Always `"ok"`; a failing process doesn't answer at all |
| `component` | `ozyd` or `agent` |
| `version` | Build version from `git describe` (`dev` for an unstamped build) |
| `uptime_seconds` | Seconds since the process started, rounded |
| `hostname` | *(agent only)* The value of the `host` tag this agent applies |
| `intake_url` | *(agent only)* Where the agent forwards to |
| `statsd_addr` | *(agent only)* The bound statsd UDP address, when enabled |

Both images use this through their own `healthcheck` subcommand, because they
contain no curl (see `internal/cli`).

### `GET /debug/vars`

The process's self-metrics (the `ozy.*` instruments). They're catalogued
in [metrics-catalog.md](metrics-catalog.md).

```console
$ curl -s localhost:8126/debug/vars
{"metrics":[{"name":"ozy.build.info","type":"gauge","tags":["component:agent","version:dev"],"value":1}, …]}
```

Each entry has `name`, `type` (`counter` | `gauge`), `tags` (sorted
`key:value` strings) and `value`. `value` is `null` for a non-finite gauge.
Entries are sorted by name, then tags.

## ozyd only

### `GET /` (web UI)

Serves the embedded single-page UI (`internal/api`):

- Existing files are served as-is. Hashed files under `/assets/` are cached
  for a year (`immutable`).
- Any other path returns `index.html` with `Cache-Control: no-cache`, so
  client-side routes like `/dashboards/abc` survive a reload and a new build
  is picked up on the next load.
- A missing file under `/assets/` is a real `404`. It means a stale page is
  asking for an old hash, and serving HTML in place of JavaScript would fail
  confusingly.
- A binary built without the UI serves a page explaining `make web`.

### `POST /v1/series`

Metric intake from agents (or any sender). Body, limits, validation and the
response are normative in [wire-protocol.md §C](wire-protocol.md#c-series-agent--ozyd-post-v1series).
In short: gzip'd JSON `{"series":[…]}`; `202` with per-series
`accepted`/`rejected` counts even when some series are refused; `400` for a
body that isn't a series payload; `413` over 4 MiB sent or 16 MiB
decompressed; `503` when a store is unavailable (retry). A metric's type is
fixed the first time it arrives; a later series with another type is
rejected.

```console
$ printf '{"series":[{"metric":"demo.count","type":"count","interval":10,"tags":["env:dev"],"points":[[%d,3]]}]}' $(date +%s) \
    | curl -s -H 'Content-Type: application/json' --data-binary @- localhost:9400/v1/series
{"status":"ok","accepted":1,"rejected":0,"errors":[]}
```

### `POST /v1/sketches`

Distribution intake from agents. Body, limits, validation and the response are
normative in [wire-protocol.md §D](wire-protocol.md#d-sketches-agent--ozyd-post-v1sketches-from-m2).
Same shape as `/v1/series`: gzip'd JSON `{"sketches":[…]}`, `202` with
per-series counts, `400` for a body that is not a sketch payload, `413` over
the size limits, `503` when a store is unavailable — or when this deployment
has no sketch store at all.

Each sketch is stored whole, and its four **exact** aggregates are also
written as ordinary series: `<metric>.count`, `.sum`, `.min` and `.max`. So a
distribution answers percentiles through the sketch and averages through
plain series, with no percentile machinery running to draw an average.

The metric is recorded with type `distribution`, which is what makes it answer
`p50`…`p99` and refuse `avg`/`sum`/`min`/`max` — those have real answers on
the four derived series, and returning one for the distribution itself would
be a different number under the same name.

### `GET /api/v1/query`, `POST /api/v1/query`

Evaluates a [metricql](query-language.md) query. That document is normative
for what a query *means*; this one covers only how a query gets in and what
comes back.

A request sends **either** `q`, the query, **or** the structured
`metric`/`filter`/`by`/`agg` parameters M1 defined — not both. Sending both is
a `400`: the request says two things and the caller is the one who should
decide which.

| Param | Default | Meaning |
|---|---|---|
| `q` | | The query, e.g. `sum:http.request.count{env:dev} by {route}.as_rate()` |
| `from`, `to` | last hour | Unix seconds, inclusive. Both in `[0, 253402300799]`, `to` after `from` |
| `interval` | ~300 points | Bucket width in seconds, at most 1500 buckets. The default is the range / 300 rounded up to a multiple of 10. A `.rollup(_, secs)` in the query sets it too, and disagreeing with it is an error rather than a resample ([ADR-0016](adr/0016-one-grid-per-query.md)) |
| `var.<name>` | | Binds the query's `$<name>` to one or more `key:value` tags. **Repeat the parameter** for a multi-select: `var.env=env:dev&var.env=env:prod` means either, not both. An empty value is dropped, so `var.env=` (a cleared selection) constrains nothing and warns — which is a dashboard's "all" |

`POST` takes the same fields as a JSON object (`{"q":…,"from":…,"to":…,
"interval":…,"vars":{"env":["env:dev"]}}`), because a generated dashboard
query outgrows a URL. `from`/`to` default the same way, and `0` is the epoch on
both verbs rather than a field that was not sent. An unknown field, trailing
content after the object, or a body over 64 KiB is a `400` that says which.

```console
$ curl -s --get localhost:9400/api/v1/query --data-urlencode \
    'q=sum:http.request.count{service:checkout} by {route}.as_rate()' \
    -d from=1790000000 -d to=1790000059 -d interval=20
{"status":"ok","query":"sum:http.request.count{service:checkout} by {route}.as_rate()",
 "from":1790000000,"to":1790000059,"interval":20,"warnings":[],
 "series":[{"metric":"http.request.count","tags":{"route":"/items"},"scope":"route:/items",
            "points":[[1790000000000,0.2],[1790000020000,null],[1790000040000,0.35]]}]}
```

- `query` is the **canonical spelling** of what was evaluated — minimal
  parentheses, keys lower-cased. For a structured request it is the query
  language equivalent, which is how somebody migrating finds out what to type.
- `points` are `[unix_ms, value]`, one per bucket, bucket starts aligned to
  multiples of `interval` (not to `from` — see the query language doc). `null`
  is an empty bucket, drawn as a gap.
- `tags` holds only the group-by keys; a group whose series lack a key omits
  it, and with no `by` it is `{}`.
- `scope` is how a legend names the line: the tags joined and sorted, or `*`
  for an ungrouped query. It is in the response so every client names a line
  the same way instead of each inventing its own join.
- `warnings` is a list of sentences for a human — a group dropped by a join, a
  variable that resolved to nothing, a rollup method ignored on a percentile.
  It is always present, empty when there is nothing to say.
- Series are sorted by `scope`, so a chart's legend is stable across requests.

Status codes:

| Code | Means | Body |
|---|---|---|
| `400` | The caller's: a parse error (whose message carries the column), or a query the evaluator refuses — a percentile of a gauge, an unbound `$var`, more than 1000 series in one node, more than 1500 buckets, a body that is malformed or too large | the reason, written to be shown to whoever typed the query |
| `499` | The caller hung up before the answer was ready. Not an error, and not logged as one — an editor that re-queries on each keystroke abandons requests constantly | `the client closed the request` |
| `503` | The server cannot answer this, now: the 30-second budget ran out, there is no sketch store for a percentile, or this metric's sketches were built at different relative accuracies | the reason, which names the metric — it is what an operator goes looking with |
| `500` | Ours | `the query could not be answered`, and nothing else; the real reason goes to the log, because a store's error names files, tables and plans |

A cancelled context is only a `499` when it is the *request's* context that was
cancelled. Work that cancelled itself while the caller was still waiting has
failed, and is a `500`.

A percentile aggregator takes a different path. It selects on
`<metric>.count`, reads the sketches of the series it finds, **merges** every
sketch of every series in the group for each output bucket, and takes the
quantile of the merged result. Merging first is the whole point: the mean of
two hosts' p95s is not the fleet's p95, and is not an approximation of it
either. Every answer is within the sketch's relative error (1% by default) of
the true value, at any magnitude. Sketches built at different relative
accuracies are refused rather than merged, since answering from whichever
subset agreed would be a confident wrong number.

#### The M1 structured parameters

Still accepted, and translated into a query rather than evaluated separately,
so there is one engine to be right.

| Param | Default | Meaning |
|---|---|---|
| `metric` | *(required)* | Metric name |
| `filter` | none | Comma-separated tag terms, ANDed: `k:v`, `k:v*`, `!k:v`, `!k:v*` |
| `by` | none | Comma-separated tag keys to group by |
| `agg` | `avg` | `avg`, `sum`, `min`, `max`, `count`, or `p50`…`p99` |

Every piece is validated against the same rules the parser would apply before
it is interpolated, because this path builds a program out of strings the
caller sent: an `agg` of `x{*}} + sum:other{*`, or a tag value containing a
brace, is a `400` rather than a query of the caller's choosing running under
parameters that describe a different one. "The same rules" includes the
lexer's lower-casing of tag keys, so `filter=Env:dev` and `q=…{Env:dev}` agree
on meaning `env:dev`.

M1's bare `k` term ("has the tag `k` with no value") has no spelling in the
query language — `k:` is a parse error on purpose — so it is widened to `k:*`
and a warning says so. Use `q=` to be exact.

Four limits changed. The 366-day cap on `to - from` is replaced by the
30-second wall-clock budget, which bounds the same thing — work — without
guessing at how much data a day holds. The 10,000-bucket cap is now 1500,
already more points than a chart draws. A filter value still cannot contain a
comma, because the comma separates terms — but it now cannot contain `{` or
`}` either, and says so rather than producing a different query.

And one limit is new, which is the one to know about: **a query node may select
at most 1000 series.** M1 had no such cap — it would aggregate a
hundred-thousand-series metric into one line and only the bucket count bounded
the answer. A bare `?metric=<something high-cardinality>` that used to return
a chart now returns `400 … selects more than 1000 series`, and the fix is a
narrower `filter` or a `by` that says which lines you actually wanted.

### `POST /api/v1/query/validate`

Does this query parse, and if not, where. Takes `{"q":"…"}` and answers `200`
either way: the request is well formed whatever the query in it turns out to
be. It exists for a query editor calling it on each keystroke, which wants a
column to underline rather than an exception.

```console
$ curl -s localhost:9400/api/v1/query/validate -d '{"q":"sum:x{a:b by {k}"}'
{"ok":false,"error":{"msg":"expected ',' or '}' but found '{'","col":14}}

$ curl -s localhost:9400/api/v1/query/validate -d '{"q":"SUM:x{ a : b } BY {K}"}'
{"ok":true,"query":"sum:x{a:b} by {k}"}
```

`col` is a 1-based byte column into `q`, and `msg` does not repeat it — the
editor already knows where it put the caret. On success, `query` is the
canonical spelling, which is what an editor's "format" produces and what a
dashboard stores.

## Dashboards

A dashboard is a stored JSON definition — what to draw, not what was drawn. The
definition's own schema is normative in [dashboards.md](dashboards.md); this
section is the HTTP around it.

Definitions are **validated on the way in and returned verbatim on the way
out**. Validation refuses anything certain to fail later: a query that does not
parse, a `$var` no `template_vars` entry declares, an unknown widget type, a
layout off the twelve-column grid. It deliberately does *not* run the queries —
a dashboard for a service that has not shipped yet is a legitimate dashboard,
and saving one should not depend on the data being there.

"Verbatim" means the definition is **stored** exactly as sent — byte for byte,
including formatting — and served back from those bytes rather than from a
re-encoding, so a dashboard exported from one ozyd and imported into another
does not pick up a diff from this build's JSON encoder. The response splices the
metadata in beside the definition's fields, so the outer braces and any
surrounding whitespace are the response's own; everything between them is the
author's.

### `GET /api/v1/dashboards`

Every dashboard, by title, definitions included.

```console
$ curl -s localhost:9400/api/v1/dashboards
{"status":"ok","count":1,"unreadable":[],"dashboards":[
  {"uid":"home","title":"Home","widgets":[…],
   "id":1,"provisioned":true,"created_at":"...","updated_at":"..."}]}
```

`dashboards` is always an array, empty rather than `null`. Each entry is the
database's columns (`id`, `provisioned`, `created_at`, `updated_at`) with the
definition's fields spliced in beside them, not nested — a client that just
fetched a dashboard wants to render it, not unwrap it.

**The database's metadata wins.** `id`, `provisioned`, `created_at` and
`updated_at` are written *after* the definition's fields, because in JSON the
last of two duplicate keys is the one a parser keeps. Nothing can put those keys
in a definition in the first place — they are not fields of one, and unknown
fields are refused on both the API and the provisioning path — but a row that
somehow contained them cannot lie about its own id.

The list carries every definition rather than a summary. Twenty dashboards is a
few tens of kilobytes, and the alternative — a list plus a fetch per row — is
what makes a dashboard picker feel slow.

`unreadable` names any rows whose stored definition could not be spliced into a
response, and is always present. Each row is encoded separately so that one
unusable definition costs its own entry rather than the whole list: the picker is
built on this endpoint, so a single hand-edited row must not become "nobody can
open anything". The reason is logged with the id; the response says only that the
row exists and cannot be rendered.

### `POST /api/v1/dashboards`

Creates one. The body is a definition. Answers `201` with the stored object and
a `Location` header, because the `id` is assigned here and the caller could not
have known it.

```console
$ curl -s -X POST localhost:9400/api/v1/dashboards -d @checkout.json
{"title":"Checkout",…,"id":2,"provisioned":false,"created_at":"...","updated_at":"..."}
```

### `GET /api/v1/dashboards/{id}`

One dashboard. `404` if there is none.

### `PUT /api/v1/dashboards/{id}`

Replaces the definition. `created_at` does not move — an edit is not a new
dashboard — and `updated_at` does.

### `DELETE /api/v1/dashboards/{id}`

`204`, with no body: there is nothing left to describe, and a body saying so is
a body every client has to decide whether to parse.

### Status codes

| Code | Means |
|---|---|
| `400` | The definition does not validate (the message names every problem, not just the first), the body is over 1 MiB, or `{id}` is not a positive integer |
| `404` | No dashboard with that id |
| `409` | This dashboard is **provisioned from a file**, so a write would be undone at the next restart. The message says to edit the file instead |
| `499` | The caller hung up; not logged as an error |
| `500` | Ours. The body says only that the request could not be completed |

A trailing slash (`/api/v1/dashboards/`) is a `404` from the router rather than
a `400` from the handler: a Go 1.22 wildcard does not match an empty segment.

### Provisioning

Directories listed in `provisioning.paths` are read at startup and upserted by
the definition's `uid`. That is why a provisioned dashboard is read-only over
HTTP: provisioning runs again at every restart, so an edit made through the API
would silently vanish, and the person to tell is the one making the edit.

Rules worth knowing:

- Only `*.json` directly in each directory. **Subdirectories are not scanned**,
  so a fixture or a work-in-progress can sit next to the real ones.
- A `uid` is **required** and is never guessed from the filename — renaming a
  file would otherwise create a second dashboard and orphan the first.
- Files are applied in sorted order within a directory, and directories in the
  order configured, so two files claiming one `uid` resolve the same way on
  every startup. A later directory wins, which is how an app's own directory
  deliberately overrides a stock dashboard.
- **One bad file does not stop the others, or startup.** The directories come
  from config and an app repo mounts its own, so a file this ozyd has never
  seen can appear because somebody deployed a different service. Refusing to
  start would make one team's typo an outage for everybody's monitoring, at the
  moment monitoring is most wanted. Each failure is logged with its path and
  reason, and counted in the `provisioned dashboards` line.
- A directory that does not exist is skipped and logged at INFO, not counted as
  a failure: `provisioning.paths` naming a directory an app has not mounted yet
  is a configuration that will become correct.
- An unchanged file is **not a write**, so `updated_at` keeps meaning "when did
  this dashboard last change" across restarts.


### `GET /api/v1/metrics`

Metric names, sorted. `?prefix=` filters by prefix (literal), `?limit=`
(1–1000, default 100) caps the list.

```console
$ curl -s 'localhost:9400/api/v1/metrics?prefix=http.'
{"metrics":["http.request.count","http.request.duration.avg"]}
```

### `GET /api/v1/tags`

The tag keys used by any series of `?metric=` (required), sorted:
`{"keys":["env","host","route"]}`.

### `GET /api/v1/tags/values`

The non-empty values of `?key=` across `?metric=`'s series (both required),
sorted, at most `?limit=` (1–1000, default 100): `{"values":["/api/comics","/api/users"]}`.
