# M1 — Metrics tracer bullet

**Goal:** one metric travels the whole path — app-node → statsd UDP → agent
aggregation → HTTP intake → naive store → query API → a chart. Every layer is
thin; every layer exists.

**Concepts learned:** why statsd is UDP; why aggregation happens at the edge;
what a flush interval does to counters vs gauges; contexts and tag cardinality;
at-most-once delivery and why that is acceptable for metrics.

## 1. SDK statsd clients (`sdk/python`, `sdk/node`)

Both packages are set up as **publishable, app-agnostic libraries** from the
first commit (metadata, README, LICENSE, types; see extensibility.md §5). The
datagram format is a compatible subset of extended StatsD, so third-party clients in
any language work against the agent — a stated guarantee with its own tests.

Identical public API shape:

```python
import ozy
ozy.init(service="app-python-api", env="dev", version="0.1.0")   # reads OZY_* env; args override
from ozy import statsd
statsd.increment("submission.created", tags=["language:python"])
statsd.decrement(name, value=1, tags=None, sample_rate=1.0)
statsd.gauge("arq.queue.depth", 3)
statsd.histogram(name, value, …)
statsd.distribution("http.request.duration", 12.4, tags=[…])
statsd.timing(name, ms, …)
statsd.set("users.unique", user_id)
with statsd.timed("judge.run.duration", tags=[…]): …      # py: ctx manager + decorator; node: async fn wrapper
```

```ts
import { init, statsd } from "ozy";
init({ service: "app-node" });
statsd.increment("http.request.count", 1, { tags: ["route:/api/comics"] });
await statsd.timed("cover.resize.duration", () => sharpWork(), { tags });
```

Behaviour (both SDKs):

- Config from env: `OZY_AGENT_HOST` (**unset → everything is a no-op**),
  `OZY_STATSD_PORT` (8125), `OZY_SERVICE`, `OZY_ENV`,
  `OZY_VERSION`, `OZY_TAGS` (comma list), `OZY_DEBUG`.
- Global tags `service`, `env`, `version` appended to every message.
- Client-side sampling: with `sample_rate < 1`, send with probability `rate`
  and include `|@rate`.
- Buffering: messages are appended to a buffer flushed when the next message
  would exceed max payload (1432 B) or every 100 ms (Node: unref'd timer;
  Python: daemon thread started lazily, restarted after fork via
  `os.register_at_fork`). `flush()` and `close()` are public; `close()` is
  registered at exit.
- Socket: UDP, non-blocking, connected lazily; DNS resolved at most once per
  60s; any send error is swallowed and counted in `statsd.stats()`.
- Client-side sanitation is minimal: in names and tags, `|`, `\n` and `,` are
  replaced with `_` so one bad value cannot corrupt the datagram. Everything
  else (lowercasing, length limits, character set) is left to the agent, which
  is the normalizer of record — one implementation instead of three.

## 2. Agent

### 2.1 `internal/agent/statsd`
- UDP listener on `:8125`, `SO_RCVBUF` raised to 4 MiB (config), N reader
  goroutines (default 2) reading into pooled 8 KiB buffers, handing datagrams
  to parser workers over a bounded channel (full → drop + count
  `ozy.agent.statsd.packets_dropped`).
- Zero-allocation-ish parser: `Parse(line []byte) (Message, error)` per
  wire-protocol §A. Benchmarked.

### 2.2 `internal/agent/aggregator`
- Bucket width 10s (config `flush_interval`). A message is assigned to bucket
  `floor(ts/10)*10` where `ts` is receive time (or `|T` if present and within
  ±60s).
- Context key: name + sorted de-duplicated tags (after normalization and after
  adding `host:` + agent-level `tags:`). Map `contextKey → aggregate`, sharded
  by hash to cut lock contention.
- Aggregates per wire-protocol §A table. Histogram keeps raw values per bucket
  (cap 10k per context per bucket; beyond that, reservoir-sample) and computes
  avg/min/max/median/p95/count at flush.
- Flush loop: each tick, close buckets with `end <= now` and emit `wire.Series`.
- **Counter zero-fill:** a counter context that receives nothing emits `0` for
  each bucket until idle for `context_expiry` (default 5 min), then is
  forgotten. (Why: otherwise a rate graph shows a gap instead of zero, and
  monitors can't tell "no errors" from "no data".) Gauges are *not* repeated.
- Self metrics: contexts count, points flushed, flush duration.

### 2.3 `internal/agent/forwarder`
- Receives `[]wire.Series`, splits into payloads of ≤ 5000 series / ≤ 2 MiB
  JSON, gzips, `POST {intake_url}/v1/series` with 10s timeout.
- Retry per wire-protocol §0: exponential backoff 1s → 60s with full jitter,
  per-payload; in-memory retry queue bounded at 64 MiB, **drop-oldest** with
  counter `ozy.agent.forwarder.dropped`. Honors `Retry-After`.
- On shutdown: one final flush attempt with a 5s budget.

## 3. ozyd

### 3.1 `internal/intake`
`POST /v1/series`: limited gzip reader → `wire.DecodeSeries` (validation per
wire-protocol §C; invalid items rejected individually) → metric type registry
(SQLite `metric_meta(metric PRIMARY KEY, type, interval, first_seen)`) →
`MetricStore.Append`. Returns the 202 body with accepted/rejected counts.

### 3.2 `internal/tsdb` — the interface everything later depends on

```go
package tsdb

type Tag struct{ Key, Value string }            // bare tag → Value == ""
type Sample struct{ T int64; V float64 }        // T = unix milliseconds
type SeriesRef struct {                          // identity
    Metric string
    Tags   []Tag                                 // sorted, unique
}
type SeriesSamples struct { Series SeriesRef; Samples []Sample }

type MatchType int                               // Equal, NotEqual, Wildcard, NotWildcard
type Matcher struct{ Key, Value string; Type MatchType }   // Value may contain '*' for wildcard types
type Selector struct{ Metric string; Matchers []Matcher }

type SeriesIterator interface {                  // one series' samples in [from,to], ascending
    Next() bool; At() Sample; Err() error
}
type SeriesSet interface {                       // series in deterministic (sorted-by-identity) order
    Next() bool; Series() SeriesRef; Iterator() SeriesIterator; Err() error; Close() error
}

type MetricStore interface {
    Append(ctx context.Context, batch []SeriesSamples) (AppendResult, error)   // per-series rejects reported, not fatal
    Select(ctx context.Context, sel Selector, fromMs, toMs int64) (SeriesSet, error)
    MetricNames(ctx context.Context, prefix string, limit int) ([]string, error)
    TagKeys(ctx context.Context, metric string) ([]string, error)
    TagValues(ctx context.Context, metric, key string, limit int) ([]string, error)
    Stats() StoreStats
    Close() error
}
```

### 3.3 `internal/tsdb/naive`
SQLite implementation: tables `series(id, metric, tags_json, key UNIQUE)`,
`series_tags(series_id, key, value)` indexed on `(key,value)`,
`samples(series_id, t, v, PRIMARY KEY(series_id,t)) WITHOUT ROWID`. Upsert on
duplicate timestamp (last write wins). WAL journal mode. This store is **kept
permanently** as the differential-test oracle (testing.md L3).

### 3.4 Minimal query API (`internal/api`)
`GET /api/v1/query?metric=&filter=k:v,k2:v2&by=k,k3&agg=avg|sum|min|max&from=&to=&interval=`
(structured params; the text language arrives in M3 and this endpoint is then
re-implemented on top of it). Semantics: select → bucket each series by
`interval` (default: range/300 rounded up to a multiple of 10s; per-bucket
reducer avg for gauge, sum for count) → group by `by` keys → apply `agg`
across series per bucket. Response:

```json
{"status":"ok","from":1790000000,"to":1790003600,"interval":20,
 "series":[{"metric":"http.request.count","tags":{"route":"/api/comics"},
            "points":[[1790000000000,4.0],[1790000020000,null]]}]}
```
Also `GET /api/v1/metrics?prefix=`, `GET /api/v1/tags?metric=`, `GET /api/v1/tags/values?metric=&key=`.

### 3.5 UI — Metrics Explorer v0
Metric-name autocomplete, filter chips with tag key/value autocomplete,
group-by, aggregator select, time-range picker (5m/15m/1h/4h/1d + absolute),
auto-refresh (10s), one uPlot line chart with legend + hover values. URL holds
the full query state.

## 4. Integrate app-node
**Colima and UDP (decided 2026-09-19):** under Colima, UDP from the Mac
doesn't reach containers (docs/operations.md). app-node runs on the host,
so **its statsd goes to a natively running agent (`make dev`)**. The owner
chose this over switching Colima to `portForwarder: grpc`, which would restart
every container on the machine. Containerized apps (app-python, app-ruby)
keep using the agent in compose. The `statsd-flood` criterion targets the
native agent too.

Also ship `examples/cron-script.sh` (metric via `nc`, no SDK) into smoke — the
first outside-in proof that the protocol is the interface.

Per `docs/private/integrations.md` §1 (M1 part): SDK vendored; `init` in
`instrumentation.ts`; `http.request.count` + `http.request.duration` emitted
from `logRequest()` in `src/lib/api.ts` with normalized `route`.

## 5. Test plan (see testing.md for layer definitions)

- **L1** statsd parser table tests (every type, rates, tags, `|T`, multi-line,
  each malformed shape); aggregator semantics per type incl. sample-rate
  scaling, bucket boundaries with fake clock, zero-fill and expiry, histogram
  stats; forwarder splitting; intake validation (each reject reason); naive
  store select/matchers (equal, not-equal, wildcard); query bucketing/grouping
  math incl. empty buckets → `null`.
- **L2** `parse(format(m)) == m`; aggregator: Σ of flushed counter values ==
  Σ of inputs/rate for any arrival pattern; tag order never changes context.
- **L4** fuzz: statsd parser, `wire.DecodeSeries`.
- **L5** forwarder vs scripted faulty server: retries on 5xx/429/timeout, no
  retry on 400, honors `Retry-After`, drop-oldest at the memory cap, final
  flush on shutdown.
- **L6** `-race` stress: 8 UDP writers × 100k packets, assert
  received + dropped == sent (loopback) and no lost increments in aggregator.
- **L7** wire goldens for hops A and C, loaded by Go + both SDKs; extended StatsD
  compatibility goldens captured from real third-party clients
  (`pkg/wire/testdata/statsd-compat/`), incl. ignored `|c:` fields and
  counted-but-dropped `_e{}` / `_sc` lines.
- **L8** in-process: UDP packet → fake-clock advance → `/api/v1/query` returns the value.
- **L10** both SDKs: full contract + safety suite (testing.md L10).
- **L9** smoke v1: `printf 'smoke.test:1|c' | nc -u -w0 localhost 8125` ×N →
  poll `/api/v1/query` until Σ == N.
- **L12** benchmarks: parser ns/op + allocs; aggregator msgs/sec; record in notes.

## 6. Docs deliverables
`docs/sdk/python.md`, `docs/sdk/node.md` (statsd sections); `docs/api.md`
(query + metadata endpoints); `docs/metrics-catalog.md` (app-node http
metrics + `ozy.agent.*`); DESIGN.md sections "Metric write path" and
"Aggregation model" with `docs/diagrams/metric-write-path.mmd`;
`docs/operations.md` agent config reference; app-node docs per its own
rules; `docs/notes/M1.md`.

## 7. Acceptance criteria

- [ ] Browsing app-node makes `http.request.count{service:app-node} by {route}` move on the Explorer within ~20s.
- [ ] With ozymandias stopped, app-node behaves identically; its full test suite passes with `OZY_AGENT_HOST` unset *and* set-but-unreachable.
- [ ] Killing ozyd for 2 min then restarting loses no flushed buckets (forwarder retry), and the agent's memory stays bounded.
- [ ] 50k statsd msgs/sec sustained on loopback for 60s with < 0.1% drops (loadgen scenario `statsd-flood`), numbers recorded.
- [ ] All test-plan items exist and pass; coverage gates green.
- [ ] Docs deliverables complete; `docs/notes/M1.md` has acceptance evidence.
