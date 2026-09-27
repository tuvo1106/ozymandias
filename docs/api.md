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
| `var.<name>` | | Binds the query's `$<name>`. **Repeat the parameter** for a multi-select: `var.env=env:dev&var.env=env:prod` means either, not both. Present with no value binds it to nothing, which is a dashboard's "all" |

`POST` takes the same fields as a JSON object (`{"q":…,"from":…,"to":…,
"interval":…,"vars":{"env":["env:dev"]}}`), because a generated dashboard
query outgrows a URL. Unknown fields are a `400`, and the body is capped at
64 KiB. `from`/`to` default the same way.

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

Status codes. `400 {"status":"error","error":"…"}` is the caller's: a parse
error (whose message carries the column), or a query the evaluator refuses —
a percentile of a gauge, an unbound `$var`, more than 1000 series in one
node, more than 1500 buckets. `503` means the query ran past its 30-second
budget, or asked for a percentile on a server with no sketch store. `500` is
ours; its body says only that the query could not be answered, and the reason
goes to the log, because a store's error names files and tables.

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
parameters that describe a different one.

M1's bare `k` term ("has the tag `k` with no value") has no spelling in the
query language — `k:` is a parse error on purpose — so it is widened to `k:*`
and a warning says so. Use `q=` to be exact.

Two M1 limits are gone. The 366-day cap on `to - from` is replaced by the
30-second wall-clock budget, which bounds the same thing — work — without
guessing at how much data a day holds. The 10,000-bucket cap is now 1500,
which is already more points than a chart draws.

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
