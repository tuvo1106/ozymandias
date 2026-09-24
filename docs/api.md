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

### `GET /api/v1/query`

The M1 structured metric query (M3 re-implements it on the query language).

| Param | Default | Meaning |
|---|---|---|
| `metric` | *(required)* | Metric name |
| `filter` | none | Comma-separated tag terms, all of which must match: `k:v`; `k:v*` (wildcard, `*` only); `!k:v` (not equal); `!k:v*`; bare `k` for a bare tag. Values can't contain `,` |
| `by` | none | Comma-separated tag keys to group by |
| `agg` | `avg` | Across-series aggregator per group: `avg`, `sum`, `min`, `max` |
| `from`, `to` | last hour | Unix seconds, inclusive |
| `interval` | ~300 points | Bucket width in seconds. The default is the range / 300, rounded up to a multiple of 10. At most 10,000 buckets |

Evaluation: select the series that pass the filters; aggregate each over
time into `interval` buckets (sum for a `count`/`rate` metric, average for a
`gauge` — the type recorded at intake); group by the `by` keys; aggregate
across each group's series per bucket with `agg`.

```console
$ curl -s 'localhost:9400/api/v1/query?metric=http.request.count&filter=service:app-node&by=route&agg=sum&from=1790000000&to=1790000059&interval=20'
{"status":"ok","from":1790000000,"to":1790000059,"interval":20,
 "series":[{"metric":"http.request.count","tags":{"route":"/api/comics"},
            "points":[[1790000000000,4],[1790000020000,null],[1790000040000,7]]}]}
```

- `points` are `[unix_ms, value]`, one per bucket, with bucket starts aligned to
  multiples of `interval`. `null` is an empty bucket, drawn as a gap.
- `tags` holds only the group-by keys; a group whose series lack a key omits
  it (and with no `by`, `tags` is `{}`).
- Series are sorted by their label, `metric{k:v,…}`.
- Errors are `400 {"status":"error","error":"…"}` for a bad request and `500`
  for a store failure.

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
