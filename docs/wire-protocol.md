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

Validation is per span (`pkg/wire` `DecodeTraces`): a span is refused whole or accepted whole, and
a refused span never takes its chunk down with it. Refused: ids that are not 32 / 16 lowercase hex or
are all zero, a `parent_id` equal to the span's own id, an empty or over-long `service` (100 bytes) or
`name` (100), `error` other than 0/1, a negative `duration`, a `start` that is not microseconds (a value
below 10^15 reads as seconds or milliseconds) or is more than 10 minutes ahead of the receiver, a
non-finite metric. Normalized, not refused: an unknown `type` becomes `custom`; `resource` and `meta`
values are cut to 5000 bytes on a rune boundary; a `meta` key over 100 bytes is dropped; a span keeps at
most 100 `meta` and 50 `metrics` entries (the rest dropped in sorted key order, so the same span always
normalizes the same way). Limits: 1000 chunks per body, 5000 spans per chunk, 10 MiB of body.

Response `200`:

```json
{"rate_by_service": {"service:app-python-api,env:dev": 1.0}, "accepted": 12, "rejected": 1}
```

`429` when the agent is already decoding 8 bodies: the SDK drops the chunk and moves on. Traces are
best-effort; nothing retries them.

SDKs apply the rates to future head-sampling decisions.

### Sampling

The head-sampling decision is made once, at the trace's first span, and is a pure function of the
trace id and the rate:

```
keep  ==  ((low 64 bits of trace_id) * 1111111111111111111  mod 2^64)  <  rate * 2^64
```

`rate <= 0` never keeps and `rate >= 1` always does. `rate * 2^64` is exact in a double (a power-of-two
scaling), so Python (`int`), Node (`BigInt`) and Go (`uint64`) agree bit for bit; the three suites check
the same vectors, `pkg/wire/testdata/traces/sampling.json`. The decision becomes `_sampling_priority`
(1 keep, 0 drop) on the local root and rides the propagation headers, so a downstream service inherits it
instead of re-deciding. A trace that is dropped by the head sampler is **still sent to the agent**: the
agent computes request/error/latency statistics on every span before it samples, so a 10% sample never
shows as 10% of the traffic (docs/notes/M5.md).

### Path normalizer

HTTP resources use the route pattern when the framework knows one; when it does not, the SDK
normalizes the raw path: a segment that is all digits, a UUID (any case), hex of 12 or more characters, or
nanoid-like (16 or more of `[A-Za-z0-9_-]` containing a digit) becomes `:id`; the query string and
fragment are dropped; at most 8 segments are kept. Vectors: `pkg/wire/testdata/traces/normalize-path.json`
(`wire.NormalizePath` in Go).

### Propagation headers (between services)

```
x-ozy-trace-id: <32 hex>
x-ozy-parent-id: <16 hex>
x-ozy-sampling-priority: <-1|0|1|2>
```

Non-HTTP carriers (arq job kwargs) use a dict with keys `trace_id`,
`parent_id`, `sampling_priority` under the kwarg `_ozymandias`. A header that is malformed in any way
(wrong length, uppercase is accepted and lowered, zero id, a priority outside -1..2) yields no context
at all: the receiver starts a fresh trace instead of continuing a corrupt one (`wire.ParsePropagation`).

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

- `interval` (seconds) is required and positive: a sketch describes a window,
  never an instant. There is no `type` field — the endpoint says what the
  payload is, and a second source of truth could only disagree with it. ozyd
  records the metric as type `distribution` in metadata on first sight.
- `ts` is the bucket start, unix seconds. The future-skew and accepted-window
  rules of §C apply unchanged.
- `gamma` travels with every sketch rather than being deployment
  configuration, because γ is what the bucket indices *mean*. Two agents at
  different relative accuracies produce indices that look alike and are not;
  a receiver that assumed its own γ would merge them into a confident wrong
  answer. ozyd refuses to merge sketches whose γ differ.
- `count`, `sum`, `min` and `max` are **exact**, not estimated, and ozyd
  writes them as the ordinary series `<metric>.count`, `.sum`, `.min` and
  `.max`. `min`/`max` are `0` when `count` is `0`.
- `bins` and `neg_bins` must **ascend by index with no duplicates**, and every
  count must be finite and positive. A decoder that sorted for the sender
  would be guessing; a duplicate index has two readings — replace or add —
  that give different percentiles from the same bytes.
- `neg_bins` index the *absolute* value, so they also ascend by index, which
  is descending by value.
- The bucket counts plus `zero_count` must equal `count` (to within
  re-summation error). A quantile resolves a rank out of `count` and then
  walks the buckets for it, so a payload where the two disagree answers with
  no error bound at all and no sign that anything is wrong.
- Limits: ≤ 5000 sketch series per request (the forwarder splits), ≤ 1000
  points per series, ≤ 2048 buckets per sign per sketch — the store's own cap,
  so a wider payload was not produced by any agent.
- Rejection is per series, as in §C: one bad series does not fail the body.

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

Validation (`wire.DecodeLogs`), per log, as for §C: a bad log is refused and the
rest of the body is kept; only a body that is not a logs payload at all is a 400.

- `ts` must be a positive unix time in **milliseconds**: a value below
  1 000 000 000 000 (before 2001) is refused as seconds, and one more than 10
  minutes ahead of the receiver's clock is refused as a clock bug. An agent-side
  retention window may refuse older logs.
- `status` must be one of `debug`, `info`, `warn`, `error`, `critical`.
- `service` is required. `service`, `source` and `host` are at most 200 bytes of
  valid UTF-8 with no control characters: they become stream labels (M4 §3).
- `tags` follow §C (at most 100, each `key:value`); they are stored sorted.
- `trace_id` is 32 and `span_id` 16 **lowercase** hex characters, when present.
- `attrs` must be a JSON object of at most 64 KiB serialized. Numbers keep every
  digit as sent (an id above 2^53 is not rounded).
- A `message` over 256 KiB is cut on a character boundary and gets
  `attrs._truncated = true`. The room for that flag is reserved inside the 64 KiB,
  so an attrs object that only fits without it is refused when the message is long.

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
examples per hop (traces: `traces/hop-b.json`, `hop-f.json`, `invalid-spans.json`, plus the shared
`sampling.json` and `normalize-path.json` vectors). Go, Python and Node test suites all load the same files
(the SDKs via a relative path) so the three implementations cannot drift.
