# Wire protocol (normative)

Every payload on every hop. `pkg/wire` holds the Go structs and validators;
both SDKs and all tests conform to this document. Version: **v1** (all HTTP
paths are prefixed `/v1`). Change this doc, `pkg/wire`, the SDKs and the
golden-file tests in one commit.

Hops:

```
SDK ──(A) statsd/UDP :8125──────────► agent
SDK ──(B) POST :8126/v1/traces──────► agent
agent ──(C) POST :9400/v1/series ───► ozyd
agent ──(D) POST :9400/v1/sketches ─► ozyd
agent ──(E) POST :9400/v1/logs ─────► ozyd
agent ──(F) POST :9400/v1/traces ───► ozyd
```

## 0. Common rules

- **This protocol is ozymandias's primary public interface** — SDKs are
  conveniences on top of it. Anything that can send UDP or HTTP can
  participate (see `docs/plan/extensibility.md` §2).
- **Compatibility:** within `/v1`, changes are additive only. Receivers MUST
  ignore unknown JSON fields and unknown statsd `|x` sections; senders MUST NOT
  rely on a field being rejected. Breaking changes ship as `/v2` alongside `/v1`.
- **HTTP bodies:** JSON, UTF-8. `Content-Type: application/json`. Agent → ozyd
  bodies are gzip'd (`Content-Encoding: gzip`); SDK → agent bodies may be.
- **Auth:** header `X-Ozy-Key: <key>`. Ignored until M7, required after
  when `auth.enabled: true`.
- **Agent identity headers** on hops C–F: `X-Ozy-Agent-Version`,
  `X-Ozy-Host`.
- **Responses:** `202 {"status":"ok","accepted":N,"rejected":M,"errors":[…≤10 strings]}`
  on success (partial acceptance is success). `400` malformed body, `401` bad
  key, `413` body > limit, `429` backpressure (with `Retry-After` seconds),
  `5xx` server fault.
- **Retry contract:** the sender retries on network error, `408`, `429`, `5xx`;
  never on other `4xx`.
- **Limits:** request body ≤ 4 MiB compressed, ≤ 16 MiB decompressed (guard
  against zip bombs with a limited reader).
- **Numbers and JavaScript:** JSON numbers must stay ≤ 2^53. Therefore ids are
  hex strings and span times are **microseconds** (not nanoseconds).

### Names and tags

- Metric name: `^[a-zA-Z][a-zA-Z0-9_.]{0,199}$`. Invalid characters are replaced
  with `_` by the agent; names that still fail are dropped and counted.
- Tag: `key:value` string, or bare `key`. Lowercased by the agent. Key
  `^[a-z][a-z0-9_.\-/]{0,99}$`; whole tag ≤ 200 bytes; ≤ 50 tags per point.
  Tags are a set: sorted and de-duplicated by the agent before hashing.
- Reserved tag keys: `host`, `service`, `env`, `version`, `source`.
- A **context** (= series identity) is `name + "|" + sorted tags joined by ","`.

## A. extended StatsD datagram (SDK → agent, UDP :8125)

```
<name>:<value>|<type>[|@<sample_rate>][|#<tag>,<tag>…][|T<unix_seconds>]
```

- Multiple messages per datagram separated by `\n`. No trailing newline required.
- Max datagram the agent reads: 8192 bytes. SDK default max payload: 1432 bytes
  (one Ethernet MTU); configurable.
- `value`: float64 decimal, except type `s` where it is an arbitrary string.
- `sample_rate`: (0,1]; counts are scaled by `1/rate`; histogram/distribution
  sample *counts* are scaled by `1/rate`.
- Optional field order after `type` is free; unknown `|x…` fields are ignored.

| type | meaning | agent aggregation per 10s bucket | emitted as |
|---|---|---|---|
| `c` | counter | Σ value/rate | `count`, interval 10 |
| `g` | gauge | last value | `gauge` |
| `s` | set | count of distinct values | `gauge` |
| `h`, `ms` | histogram / timer | local stats | `<name>.avg/.min/.max/.median/.95percentile` gauges + `<name>.count` count |
| `d` | distribution | DDSketch | sketch (hop D) — from M2; in M1 treated as `h` |

Parse errors drop only the offending line and increment
`ozy.agent.statsd.parse_errors`. A value that isn't a finite number
(`NaN`, `Inf`) is a parse error, and so is an empty name, value or tag
section, or a sample rate outside (0,1].

### What a client sends (both SDKs; any client may do the same)

- **Section order:** `name:value|type`, then `|@rate` (only when rate < 1), then
  `|#tags`. The agent accepts any order; the SDKs write this one so their
  output is byte-for-byte testable.
- **Numbers (canonical form):** the shortest decimal digits that round-trip a
  float64, written **positionally when the decimal exponent is in `[-4, 16)`**
  — so `1`, not `1.0`; `12.4`; `0.30000000000000004`; `1048576`;
  `1000000000000000` — and in **exponent form outside that window**, with a
  signed exponent of at least two digits: `1e+16`, `1e-05`, `5e-324`,
  `1.7976931348623157e+308`. `-0` is written `0`. NaN and ±Inf are dropped
  client-side, never sent, and so is a value that is not a number at all
  (`null`, `[]`, `"5"`): a client coerces nothing.

  The window is the only part a client has to think about, and it is there
  because each language's default float-to-string disagrees about it.
  JavaScript's `String` stays positional to `1e21` and down to `1e-7` and
  writes an unpadded exponent; Go's `strconv` `'g'` with shortest precision
  reaches for an exponent at `1e6`, turning a byte count of `1048576` into
  `1.048576e+06`. Python's `repr` is the one that already matches, so the rule
  is stated in its terms. All three forms parse back to the same float64 —
  the agent uses `strconv.ParseFloat` and accepts any of them — so this is a
  contract between clients, not a parsing requirement. It exists so that the
  shared goldens can compare bytes at all.
- **Tags:** the call's tags, then the `init()`/`OZY_TAGS` tags, then
  `service:`, `env:` and `version:` for whichever of those are set.
- **Sampling:** with `sample_rate < 1`, send when `random() < rate` and
  write `|@rate`.
- **Sanitation:** `|`, `,` and newline become `_` in names, tags and set
  members. `:` also becomes `_` in names, since it ends the name. That is all
  a client does; the agent normalizes the rest.
- `timing` sends `|ms`; `decrement(n)` sends `-n|c`.

The exact bytes for each call are in `pkg/wire/testdata/statsd/sdk-cases.json`,
which the Go parser tests and both SDK test suites load.

Examples:

```
http.request.count:1|c|#service:app-node,env:dev,route:/api/comics,method:get,status:200
http.request.duration:12.4|d|#service:app-node,env:dev,route:/api/comics
arq.queue.depth:3|g|#service:app-python-api
users.unique:user_91|s
```

## B. Traces, SDK → agent (`POST :8126/v1/traces`)

```json
{
  "tracer": {"lang": "python", "lang_version": "3.12.4", "version": "0.1.0"},
  "traces": [ [ <span>, <span>, … ], … ]
}
```

Each inner array is a **chunk**: all spans of one trace finished by one
process, flushed together when that process's local root span finishes.

### Span

| field | type | notes |
|---|---|---|
| `trace_id` | string | 32 lowercase hex chars (128-bit; W3C-compatible) |
| `span_id` | string | 16 lowercase hex chars, non-zero |
| `parent_id` | string \| null | 16 hex; null for the trace root |
| `service` | string | ≤ 100 chars |
| `name` | string | operation, low cardinality: `http.request`, `postgres.query`, `arq.job` |
| `resource` | string | what was operated on: `POST /api/v1/submissions`, SQL text, job name. ≤ 5000 chars, truncated by agent |
| `type` | string | `web` \| `db` \| `cache` \| `queue` \| `http` \| `worker` \| `custom` |
| `start` | int | unix **microseconds** |
| `duration` | int | microseconds, ≥ 0 |
| `error` | int | 0 or 1 |
| `meta` | object<string,string> | tags. Conventional keys below |
| `metrics` | object<string,number> | numeric tags. Conventional keys below |

Conventional `meta` keys: `env`, `version`, `http.method`, `http.url` (no
query string), `http.route`, `http.status_code`, `error.type`, `error.message`,
`error.stack`, `db.system`, `db.name`, `queue.name`, `job.id`, `container.image`,
`span.kind` (`server`|`client`|`producer`|`consumer`|`internal`).

Conventional `metrics` keys: `_sampling_priority` (-1 user drop, 0 auto drop,
1 auto keep, 2 user keep; set on the local root), `_top_level` (1 if the span
is a service entry span), `_measured` (1 to force stats), `queue.wait_ms`.

Response `200`:

```json
{"rate_by_service": {"service:app-python-api,env:dev": 1.0}}
```

SDKs apply these rates to future head-sampling decisions (M5 §sampling).

### Propagation headers (between services)

```
x-ozy-trace-id: <32 hex>
x-ozy-parent-id: <16 hex>
x-ozy-sampling-priority: <-1|0|1|2>
```

Non-HTTP carriers (arq job kwargs) use a dict with keys `trace_id`,
`parent_id`, `sampling_priority` under the kwarg `_ozymandias`.

From M8 the SDKs also accept and emit W3C `traceparent` (the 32-hex trace id
and 16-hex span id map 1:1) so OpenTelemetry-instrumented services join the
same trace.

## C. Series, agent → ozyd (`POST /v1/series`)

```json
{
  "series": [
    {
      "metric": "http.request.count",
      "type": "count",
      "interval": 10,
      "tags": ["env:dev", "host:host-1", "route:/api/comics", "service:app-node"],
      "points": [[1790000000, 42.0], [1790000010, 17.0]]
    }
  ]
}
```

- `type`: `count` | `rate` | `gauge`. `interval` (seconds) required for
  `count`/`rate`, 0 for `gauge`.
- `points`: `[unix_seconds, float64]`. NaN/Inf rejected. Timestamps more than
  10 min in the future rejected; older than the store's accepted window
  rejected (see M2 out-of-order rule).
- `host` is just a tag. The agent always adds `host:<hostname>` unless the
  point already has a `host` tag.
- ≤ 5000 series per request; the forwarder splits.
- ozyd records `(metric → type, interval)` in metadata on first sight;
  a later conflicting type is rejected and counted.

## D. Sketches, agent → ozyd (`POST /v1/sketches`) — from M2

```json
{
  "sketches": [
    {
      "metric": "http.request.duration",
      "tags": ["env:dev", "host:host-1", "route:/api/comics", "service:app-node"],
      "interval": 10,
      "points": [
        {
          "ts": 1790000000,
          "sketch": {
            "gamma": 1.0202020202,
            "count": 57, "sum": 803.1, "min": 2.1, "max": 96.0,
            "zero_count": 0,
            "bins": [[35, 4], [36, 9], [41, 44]],
            "neg_bins": []
          }
        }
      ]
    }
  ]
}
```

`bins` are `[index k, count]` sorted by k, where bucket k covers
`(γ^(k-1), γ^k]`. See `docs/plan/M2-tsdb.md` §DDSketch.

## E. Logs, agent → ozyd (`POST /v1/logs`)

```json
{
  "logs": [
    {
      "ts": 1790000000123,
      "message": "api",
      "status": "info",
      "service": "app-node",
      "source": "winston",
      "host": "host-1",
      "tags": ["env:dev", "version:1.2.0"],
      "attrs": {"tag": "api", "method": "GET", "path": "/api/comics", "status_code": 200, "ms": 12},
      "trace_id": "…32 hex…",
      "span_id": "…16 hex…"
    }
  ]
}
```

- `ts`: unix **milliseconds**.
- `status`: normalized to `debug|info|warn|error|critical`.
- `attrs`: arbitrary JSON object from structured logs, ≤ 64 KiB serialized,
  nesting flattened with `.` at query time, not at ingest.
- `message` ≤ 256 KiB; longer is truncated with `attrs._truncated = true`.
- ≤ 1000 logs per request.
- An app field that collides with a reserved one is moved: the app's `status`
  → `attrs.status_code` when numeric (app-node's `api` lines do this).

## F. Traces, agent → ozyd (`POST /v1/traces`)

```json
{"env": "dev", "host": "host-1", "spans": [ <span>, … ]}
```

Flat list; spans as in §B, after agent normalization and sampling. Spans of a
trace may arrive across many requests and from many agents; the TraceStore
assembles by `trace_id`. RED statistics do **not** travel here — the agent
emits them as ordinary series/sketches (`trace.<span.name>.hits`, `.errors`,
`.duration`) on hops C/D.

## Golden files

`pkg/wire/testdata/*.json` and `*.statsd` hold one valid and several invalid
examples per hop. Go, Python and Node test suites all load the same files
(the SDKs via a relative path) so the three implementations cannot drift.
