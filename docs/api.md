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

### `POST /v1/logs`

Log intake from agents. Body, limits and validation are normative in
[wire-protocol.md §E](wire-protocol.md#e-logs-agent--ozyd-post-v1logs): gzip'd JSON
`{"logs":[…]}`, at most 1000 logs. `202` with per-log `accepted`/`rejected`
counts (a bad log is refused and the rest kept); `400` for a body that is not a
logs payload or has more than 1000 logs; `413` over the size limits; `503` when
the store cannot take the batch — **nothing was stored or published, so the
agent sends the whole batch again**; also `503` when this deployment has no log
store.

A `202` means the batch is durable: it was written to the log store's WAL and
fsynced. That is the acknowledgement an agent commits its file offsets on.

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

### `POST /api/v1/query/batch`

A whole dashboard in one request. Takes a list of queries and **one** window,
interval and set of template variables:

```console
$ curl -s localhost:9400/api/v1/query/batch -d '{
    "queries": [
      {"q": "sum:http.request.count{service:api,$env} by {route}"},
      {"q": "avg:http.request.count{service:api,$env}"},
      {"metric": "queue.depth", "agg": "max"}
    ],
    "from": 1790000000, "to": 1790003600,
    "vars": {"env": ["env:prod"]}}'
{"status":"ok","from":1790000000,"to":1790003600,"results":[
  {"index":0,"status":"ok","query":"sum:http.request.count{service:api,$env} by {route}",
   "interval":20,"series":[…],"warnings":[]},
  {"index":1,"status":"ok","query":"avg:http.request.count{service:api,$env}",
   "interval":20,"series":[…],"warnings":[]},
  {"index":2,"status":"error","query":"max:queue.depth{*}","interval":0,"series":[],"warnings":[],
   "code":400,"error":"queue.depth selects more than 1000 series; narrow the filter, …"}]}
```

Each entry of `queries` is spelled like a single query — `q`, or the M1
`metric`/`filter`/`by`/`agg` parameters, which are translated the same way — and
nothing else. There is no per-query window: **the window, the interval and the
variables belong to the batch**, because two queries planned onto different
grids share no work and a dashboard has one time picker (ADR-0016, ADR-0018).

**Every query gets its own result, and its own failure.** `results` is in
request order and each entry repeats its `index`, so a result survives being
handed to the widget that asked for it. `status` is `ok` or `error`; `series`
and `warnings` are always present, empty rather than absent; `code` and `error`
appear only on a failure. `code` is the status the same query would have been
answered with on `/api/v1/query` — `400` the query's fault, `503` out of time or
not answerable by this server, `500` ours, with the message replaced and the
real one logged — so a client has one rule for both endpoints.

The HTTP status describes the *request*, not the queries in it: `400` for a body
that is not a batch, no queries, more than 50 of them, or a window that cannot
be planned; `200` otherwise, even
when every query failed. One typo in one widget must not blank a dashboard
(ADR-0017).

`interval` is **per result**, not per batch. Almost always every result shares
the batch's, but `.rollup(method, seconds)` sets the grid of the query it is
written on (ADR-0016), so `sum:x{*}` and `sum:x{*}.rollup(sum, 600)` in one
batch are evaluated on 10s and 600s buckets and each says so. It is `0` on a
failure: there is no grid, and inventing one would be worse than saying nothing.
A query with its own rollup shares no work with its neighbours, which is a
reason not to write one rather than a reason for the answer to be wrong.

A **bad window is one request-level `400`**, not a per-query error repeated
fifty times: `to` must be after `from`, both within `[0, 2^40)`, and `interval`
must not be negative. The bucket count is not checked up front, because it
depends on the interval the planner settles on.

**One deadline for the batch,** not one per query: fifty queries of thirty
seconds each is twenty-five minutes. A batch that runs out of time reports the
queries that answered and fails the rest with `503` and "the batch ran out of
time before this query was evaluated" — the fix is to ask for fewer things at
once, not for a shorter window.

Queries with the same selector, post-filter, grid and rollup are selected and
bucketized **once** for the whole batch, which is the reason to send them
together rather than in parallel requests. The saving is real — 2.2× on six
dashboard-shaped queries over a thousand series — and bounded: the cache is
capped at 16 MiB per request, after which the batch keeps answering without
sharing.

### `GET /api/v1/query/sketch`, `POST /api/v1/query/sketch`

The distribution behind a metric, per bucket, rather than a number taken from
it — the heatmap widget's endpoint. Takes a `dist:` query, the same filter,
`by`, template variables and window rules as `/api/v1/query`, on either verb.

```console
$ curl -s 'localhost:9400/api/v1/query/sketch?q=dist:http.request.latency%7Bservice:api%7D&from=1790000000&to=1790003600'
{"status":"ok","query":"dist:http.request.latency{service:api}",
 "from":1790000000,"to":1790003600,"interval":20,"bins":418,
 "series":[{"metric":"http.request.latency","tags":{},"scope":"*",
   "buckets":[{"t":1790000000000,"gamma":1.02020202020202,
               "count":51,"sum":6.13,"min":0.004,"max":1.9,
               "bins":[[0.0039,0.004,43],[0.0972,0.0991,7],[1.86,1.9,1]]}]}],
 "warnings":[]}
```

Each bin is `[lower, upper, count]`: the half-open value range `(lower, upper]`
and how many observations fell in it. **Resolved bounds, not the sketch's bucket
index** — the index is meaningless without γ and the convention that γ^k is the
bucket's *upper* bound, and a negative value's index is of its absolute value,
so ascending index would be descending value. Bins arrive in **value order**,
negatives first, and zero is its own bin `[0, 0, n]` because log γ 0 is
undefined.

`gamma` is **per bucket**, and is the error bar on that bucket's bins:
α = (γ-1)/(γ+1). Per bucket rather than per response because every sketch merging
into one bucket agrees on γ — a group whose sketches disagree is refused with
`503` — but two buckets need not, and neither need two groups: reconfigure one
host's relative accuracy and `dist:lat{*} by {host}` legitimately returns both.
A client that wants one error bar for the axis should take the largest.

`count`, `sum`, `min` and `max` are **exact**: a sketch carries them beside its
bins rather than estimating them, so a tooltip can show the real mean and the
real maximum next to an approximate shape.

A bucket nothing landed in is **absent**, not empty: a heatmap wants a gap
where there was no traffic, every bucket carries its own `t`, and 1500 empty
objects saying "nothing happened" would be most of the response on a quiet
metric.

`bins` at the top level is the total across every series, so a caller can see
how close it came to the limit. A response is refused past **200000 bins** with
`400` — a sketch's bin count grows with the *ratio* between its largest and
smallest value, not with how many observations there were, so a wide metric over
many buckets is an enormous answer nobody asked for. The message names the dials:
a coarser interval, a narrower filter, or fewer groups.

Refusals worth knowing:

- a query that is not `dist:` → `400`, naming `/api/v1/query`;
- `dist:` on `/api/v1/query` → `400`, naming this endpoint and the percentiles;
- an expression rather than one query (`dist:a{*} / dist:b{*}`) → `400`: merging
  is the only operation two distributions support;
- a metric that is not a distribution → `400`;
- no sketch store configured → `503`. Every query here needs one, where on
  `/api/v1/query` only a percentile does.

See [ADR-0019](adr/0019-the-dist-aggregator.md) for why `dist` is an aggregator.

## Logs

Four endpoints over the log store. They share a query string:

| Param | Default | Meaning |
|---|---|---|
| `q` | all logs | A logql query ([query-language.md](query-language.md#logql)): `service:web-api status:error "timeout" @route:/orders -@user:bot` |
| `from`, `to` | last 15 minutes | Unix **milliseconds**, inclusive on both ends — not seconds as in the metrics API: a log's timestamp, a histogram bucket and a cursor are all milliseconds, so a page fed back as a window needs no conversion. `from` after `to` is a `400` |

`400` is a query that does not parse (the message carries the column), a bad
parameter, or a cursor that did not come from this API. `500` is ours and says
only that the request failed.

### `GET /api/v1/logs`

A page of logs, newest first.

| Param | Default | Meaning |
|---|---|---|
| `limit` | 100 | At most 10000 |
| `order` | `desc` | `desc` or `asc` |
| `cursor` | | The previous page's `cursor`. Paging is stable while ingest continues: no log repeats or is skipped (the order is timestamp then arrival) |

```json
{"logs":[{"ts":1790000000123,"message":"db down","status":"error","service":"web-api","source":"winston","host":"box","tags":["env:dev"],"attrs":{"status_code":503,"route":"/orders"}}],
 "cursor":"…", "truncated":false,
 "stats":{"streams":2,"blocks_read":3,"bytes_read":18204,"entries_examined":412}}
```

`cursor` is absent on the last page. `truncated: true` means the **scan budget**
(`logs.scan_budget`) ran out first: what came back is a correct prefix in the
requested order and `cursor` continues from it, but logs further along were not
examined. `stats` say what the query cost: the label index selects `streams`,
then the scan decompresses `blocks_read` blocks. A query that names a service
and a status reads few; a bare word with nothing else to narrow it reads
everything in the range.

### `GET /api/v1/logs/aggregate`

A histogram of matching logs, for the bars above the list.

| Param | Default | Meaning |
|---|---|---|
| `interval` | about 60 bars | Bar width in **milliseconds**. Absent, the smallest of 1s, 5s, 10s, 30s, 1m, 5m, 10m, 30m, 1h, 3h, 6h, 12h, 24h that draws at most 60 bars. More than 5000 bars is a `400`. Bars align to multiples of the interval, so two queries draw the same bars |
| `by` | none | Split each bar: a label (`service`, `source`, `host`, `env`, `status`) or `@attr.path` |

```json
{"interval_ms":30000,"buckets":[{"ts":1790000010000,"counts":{"info":12,"error":1}}],"truncated":false,"stats":{…}}
```

Buckets are sparse: only bars that hold a log appear, in time order. A log with
no value for `by` counts under `""`.

### `GET /api/v1/logs/facets`

The most frequent values of some keys among the matching logs, for the sidebar.

| Param | Default | Meaning |
|---|---|---|
| `keys` | *(required)* | Comma separated, 1 to 10: labels or `@attr.path` |
| `limit` | 10 | Values per key, 1 to 100, most frequent first (ties by value) |

```json
{"facets":{"status":[{"value":"info","count":9},{"value":"error","count":3}]},"capped":[],"truncated":false,"stats":{…}}
```

`capped` lists keys with more than 1000 distinct values: past that a facet stops
tracking new values, so counts for late-appearing ones may be low. A log that
lists a value twice (an array) counts it once.

### `GET /api/v1/logs/tail`

Live tail as **server-sent events**: only logs that arrive after the connection
opens and match `q` (`from`/`to` are ignored). A client that wants the recent
past reads `/api/v1/logs` first and then tails; it may see a log in both.

```text
: connected

event: log
data: {"ts":1790000000123,"message":"db down", …}

event: dropped
data: {"dropped":42}

: keep-alive
```

Each subscriber has a buffer of 1000. A reader that falls behind **loses logs
rather than slowing ingest**; the loss is reported by an `event: dropped` with
the count since the last notice, sent with the next heartbeat. A comment line
(`: keep-alive`) goes out every 15 seconds so proxies keep the connection.
`503` when 64 tails are already open, or this server has none.

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
formatting included — and served back from those bytes rather than from a
re-encoding. Be precise about what that preserves, because a response is not the
stored bytes. Preserved: the definition's **key order**, every **number exactly as
written** (a decode-and-re-encode would make `12345678901234567890` into
`1.2345678901234567e+19`), and any field this build does not know about. Not
preserved: whitespace and HTML escaping — Go's JSON encoder compacts a response
and escapes `<`, `>` and `&` inside strings, so a pretty-printed definition comes
back minified with `a <b>` as `a \u003cb\u003e`. That is the response's encoding,
not the database's; the stored row is untouched, and `GET` of a definition stored
from a file returns the same fields, order and values it went in with. The
response splices the metadata in beside the definition's fields, so the outer
braces are the response's own; everything between them is the author's.

### `GET /api/v1/dashboards`

Every dashboard, by title, definitions included.

```console
$ curl -s localhost:9400/api/v1/dashboards
{"status":"ok","count":1,"dashboards":[
  {"uid":"home","title":"Home","widgets":[…],
   "id":1,"provisioned":true,"created_at":"...","updated_at":"..."}],
 "unreadable":[]}
```

`dashboards` is always an array, empty rather than `null`. Each entry is the
database's columns (`id`, `provisioned`, `created_at`, `updated_at`) with the
definition's fields spliced in beside them, not nested — a client that just
fetched a dashboard wants to render it, not unwrap it.

`count` is the length of `dashboards` — the rows this response could encode, not
the number of rows in the database. `count: 2` alongside `unreadable: [5]` means
three rows exist.

**The database's metadata is the database's.** `id`, `provisioned`, `created_at`
and `updated_at` come from the columns (the two timestamps are RFC 3339 in UTC,
whatever zone the server runs in), and a stored definition containing any of
those four keys is **refused**: it is named in `unreadable` (or answered with a
`500` on a single-dashboard `GET`) and the reason is logged with its id. Refused
rather than overridden, because a response carrying the same key twice means
whatever the reader's parser does with it — most keep the last, and some, Go's own
`encoding/json/v2` among them, reject the document outright. Nothing can put those
keys in a definition in the first place (they are not fields of one, and unknown
fields are refused on both the API and the provisioning path), so such a row is a
hand-edited database; the useful answer to one is to say which row and why.

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

### `GET /api/v1/dashboards/services`

The services a template dashboard can be instantiated for.

```console
$ curl -s localhost:9400/api/v1/dashboards/services
{"status":"ok","count":2,"services":["checkout","web"],"truncated":false,"unreadable":[]}
```

**Where this list comes from, exactly.** For every stored dashboard with
`"template": true`, the metrics its own queries name, looked up in the tag index
for the values of its `service` variable's tag key. It is therefore *"services
the store still holds one of this template's metrics for"* — **not** "services
seen in the last day", which nothing in ozymandias can answer: the tag index
takes no time range, and the metadata database tracks metrics rather than
services. [ADR-0020](adr/0020-services-come-from-the-tag-index.md) has the
alternatives and their costs. Two things follow:

- A service that has stopped reporting stays in the list until its last series
  falls out of retention. Its dashboard draws empty charts, which is what any
  dashboard of a dead service does.
- A service that reports none of the template's metrics is **not** in the list.
  That is deliberate — its instance would be a grid of empty charts with nothing
  to explain why.

`services` is always an array, sorted, deduplicated across templates, and empty
rather than `null`. `truncated` says the answer is partial, so a client can tell
"these are all of them" from "these are the ones it got to". Two things set it:
the list is capped at **1000 services** (the same limit a query node's series
selection has), and discovery is capped at **500 tag-index lookups per request**
across every template.

The lookup cap is what stops this endpoint being an amplifier. A template may
hold 100 widgets × 10 queries, and nothing bounds how many templates exist —
anybody who can `POST` a dashboard can mark one — so uncapped, a single `GET`
could ask the store about tens of thousands of metrics. It counts the **distinct**
`(series, tag key)` pairs, not the queries: templates are expected to overlap —
the shape provisioning is built for is several app repos each mounting a
directory beside the stock one, all drawing `http.request.count` — so the same
pair asked for by twenty templates is one lookup. It is far above what a real
deployment reaches: the shipped template names two metrics. Instantiation itself
needs no lookups, so `/dashboards/service/{name}` still returns every template
even for a request that hit the cap.

`unreadable` names rows that say `"template": true` and do not validate. Such a
row cannot be created through the API or by provisioning, so it is a hand-edited
database — and a template that appears nowhere is the least debuggable outcome
available, which is the whole reason the rule about declaring `service` exists.
The reason is logged once per row, with its id. A row that is *not* a template
and does not parse is not reported here; `GET /api/v1/dashboards` is where a row
nobody can read belongs.

### `GET /api/v1/dashboards/service/{name}`

Every template, instantiated for one service.

```console
$ curl -s localhost:9400/api/v1/dashboards/service/checkout
{"status":"ok","service":"checkout","count":1,"unreadable":[],
 "dashboards":[
   {"template_id":2,"template_uid":"service","service":"checkout",
    "dashboard":{"title":"Service overview: checkout",
                 "template_vars":[{"name":"service","tag":"service","default":"checkout"},…],
                 "widgets":[…]}}]}
```

A **list**, because nothing says a deployment has one template: an app repo
mounting its own provisioning directory beside the stock one is exactly the case
provisioning is for. Zero templates and five are then the same shape, and a
client that wants "the" service dashboard takes the first.

`template_id` and `template_uid` say which stored template an instance came from,
so that "this chart is wrong" leads to the file to edit.

**Every** template is instantiated, including one whose metrics this service does
not report. A template is an overview, and a service with no database is a
legitimate empty widget — not a reason to hide the dashboard that has its
throughput on it.

**The instance is bound, not rewritten.** Its `service` variable's `default` is
the service name; the queries still say `$service`, and the evaluator resolves
it per request as it does for any other variable — so the definition a reader
sees and the query the server runs are the same string. `template` and `uid` are
cleared: an instance is not itself instantiable, and nothing stores it, so a
`uid` would promise a lookup that cannot work. The title gains `": <service>"`,
capped at 200 bytes with the *title* losing its tail rather than the name.

**Unlike a stored dashboard, an instance is not the author's bytes.** It is this
build's re-encoding of the definition with one default changed, so the "verbatim"
guarantee above does not apply to it — key order and unknown fields are not
preserved. That is also why the definition is **nested** under `dashboard` rather
than spliced beside the provenance: there is no `id`, `created_at` or
`provisioned` to splice, and nothing here is byte-for-byte anyone's.

An instance is a definition `POST /api/v1/dashboards` would accept, which is what
makes "save a copy of this" possible.

`404` if no template's metrics carry that service — a typo in a URL somebody
pasted into a runbook would otherwise render a grid of empty charts, which reads
as "the service is down" rather than "the service is misspelt".

This endpoint stops looking the moment the name turns up, so the usual cost is
one tag-index lookup rather than the whole sweep `/dashboards/services` does. If
it searched everything and still gave up because a **budget** ran out, the `404`
says which one — "more than 1000 services" and "more than 500 distinct metrics
between the templates" are different problems, and the fix for one is not the fix
for the other.

Both endpoints answer `503` on a server with no metric store wired: there is
nothing to discover services from, and "no services" would be a lie.

### Status codes

| Code | Means |
|---|---|
| `400` | The definition does not validate (the message names every problem, not just the first), the body is over 1 MiB, `{id}` is not a positive integer, or `{name}` is longer than a whole tag (200 bytes) |
| `404` | No dashboard with that id, or no such service on `/dashboards/service/{name}` |
| `409` | This dashboard is **provisioned from a file**, so a write would be undone at the next restart. The message says to edit the file instead |
| `499` | The caller hung up; not logged as an error |
| `500` | Ours. The body says only that the request could not be completed |
| `503` | This server has no metric store, so templates cannot be instantiated |

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

### `GET /api/v1/metrics/cardinality`

Metrics by number of series, highest first — the cardinality view
([ADR-0023](adr/0023-series-counts-come-from-the-index.md)). `?prefix=`
filters (literal); `?limit=` (1–1000, default 100) bounds the response, but
**every** matching metric is counted, because "the highest" is a question
about all of them. `total` is how many matched and `truncated` says the list
stopped short of it. Ties are in name order.

```console
$ curl -s 'localhost:9400/api/v1/metrics/cardinality?prefix=http.&limit=2'
{"metrics":[{"name":"http.request.count","type":"count","series":120},
            {"name":"http.request.duration.count","type":"count","series":40}],
 "total":5,"truncated":true}
```

- A series is a distinct set of tags, counted once however many places the
  store keeps it in — the in-memory head and any number of blocks.
- Counts read the index, never samples, and have **no time range**: a series
  counts until retention drops it. A metric that stopped exploding yesterday
  still shows yesterday's number.
- `type` is the kind the metric was first seen with, or `null` when ozyd has
  no record of one. Null is not a kind, and is not written as `""`.
- A distribution's sketches are not counted here; its series are its
  `.count`, `.sum`, `.min` and `.max` metrics.

### `GET /api/v1/tags/cardinality`

One metric's tag keys (`?metric=`, required), each with the number of the
metric's series that carry it and the number of distinct values it takes,
most values first — the key with the most values is usually why a metric is
high. `series` is the metric's own count, so an empty `keys` means either
"its series carry no tags" (`series > 0`) or "no such metric" (`series: 0`).
A bare tag (`canary`, no value) counts toward a key's `series`, not its
`values`.

```console
$ curl -s 'localhost:9400/api/v1/tags/cardinality?metric=http.request.count'
{"metric":"http.request.count","type":"count","series":120,
 "keys":[{"key":"route","series":120,"values":40},{"key":"env","series":100,"values":2}]}
```

Both endpoints answer a store failure with `500 the series counts could not
be read`; the store's own error goes to the log.
