# M5 — Tracing / APM

**Goal:** distributed traces from both SDK apps (app-ruby gets traces in M8 via OpenTelemetry — no Ruby SDK is built), including one trace that crosses
app-python's Redis queue: `POST /submissions → enqueue → arq worker → docker run
judge → verdict write`. RED metrics computed from spans, sampling, a trace
store, flame graphs, a service map, and log ↔ trace correlation.

**Concepts learned:** the span model; in-process context propagation
(`contextvars`, `AsyncLocalStorage`) and cross-process propagation (headers,
queue payloads); how auto-instrumentation patches libraries and why bundlers
fight it; why stats are computed *before* sampling; head vs tail sampling;
secondary-index design on a KV store.

## 1. Tracer core (both SDKs — same shape)

```python
from ozy import tracer
with tracer.trace("judge.run", resource=language, service=None, type="worker") as span:
    span.set_tag("problem.id", pid); span.set_metric("tests.count", n)
@tracer.wrap("cover.resize")                      # decorator; works on sync + async
tracer.current_span(); tracer.current_trace_context()
tracer.inject(carrier: dict); tracer.extract(carrier) -> Context | None
tracer.start_span(name, child_of=ctx, activate=True)   # manual API
```

```ts
await tracer.trace("sqlite.query", { resource: sql, type: "db" }, async (span) => { … });
tracer.wrap(name, opts, fn); tracer.scope().active(); tracer.inject(ctx, headers); tracer.extract(headers);
```

- **Context:** Python `contextvars.ContextVar` (async-safe; survives `await`,
  copied into `asyncio.create_task`, *not* into threads — provide
  `tracer.wrap_executor`). Node `AsyncLocalStorage`.
- **Span lifecycle:** ids random (128-bit trace, 64-bit span, from `os.urandom`
  / `crypto.randomBytes`, never zero). `start` from wall clock, `duration`
  from the monotonic clock. Exiting with an exception sets `error=1` +
  `error.type/message/stack` and re-raises unchanged. `finish()` is idempotent.
- **Trace buffer:** spans of a trace accumulate in a per-trace buffer held by
  the local root; when the local root finishes, the chunk is handed to the
  writer. Partial flush when a trace holds > 500 finished spans. Spans
  finished after their root (detached tasks) flush as a late chunk.
- **Head sampling:** decided once at the trace root: `priority = 1` with
  probability `rate` (from `OZY_TRACE_SAMPLE_RATE`, default 1.0, then
  overridden per service by the agent's `rate_by_service` response), else `0`.
  Deterministic on trace id (`trace_id_low64 * KNUTH % 2^64 < rate * 2^64`) so
  every service agrees. Extracted contexts inherit the upstream priority.
  **Unsampled traces are still sent to the agent** (it needs 100% for stats)
  and dropped there.
- **Writer:** bounded queue (1000 chunks, drop-oldest + counter); background
  flusher every 1s or 100 chunks; `POST :8126/v1/traces`, 2s timeout, no
  retries (traces are best-effort); reads `rate_by_service`. Python: daemon
  thread, fork-safe. Node: unref'd timer. Flush on exit with 1s budget.
- `_top_level=1` set on spans whose parent is absent or belongs to another
  service. `service`, `env`, `version` defaulted from `init`.
- Config: `OZY_TRACE_ENABLED` (true), `OZY_TRACE_PORT` (8126),
  `OZY_TRACE_SAMPLE_RATE`.

## 2. Integrations

All integrations implement the public `Integration` protocol
(`name`, `is_available()`, `patch()`, `unpatch()`), are registered through the
public `register_integration()`, and use **only the public tracer/statsd API** —
so a third party can write one with exactly the same power. The tables below
are what the owner's apps need first; the wider generic set (WSGI/Flask/Django,
`requests`, psycopg, Express, Fastify, `pg`, ioredis, pino, structlog) is
listed in extensibility.md §5 and built here as capacity allows, each an
isolated module. `examples/flask-app` and `examples/express-app` prove the SDKs
on stacks the owner's apps don't use; `examples/go-app` posts spans with no
SDK at all.

**Python (`ozymandias/integrations/`)** — each is `patch()`-able individually;
`ozy.init(integrations=[…])` or `patch_all()`; each imports its target
lazily and no-ops if absent.

| Integration | Mechanism | Span |
|---|---|---|
| `asgi` | pure ASGI middleware `TraceMiddleware(app)` | `http.request`, type `web`, resource `"<METHOD> <route pattern>"` read from `scope["route"].path` **after** the inner app returns (routing hasn't happened before); extracts propagation headers; `http.status_code`; 5xx → error; also emits the M3 request metrics so one middleware does both |
| `sqlalchemy` | `event.listen(engine.sync_engine, "before_cursor_execute"/"after_cursor_execute"/"handle_error")` | `postgres.query`, type `db`, resource = statement text (**never parameters**), truncated 2000 chars; `db.rowcount` |
| `redis` | wrap `redis.asyncio.Redis.execute_command` + pipeline `execute` | `redis.command`, resource = command name only (no keys/args) |
| `httpx` | wrap `AsyncClient.send` / `Client.send` | `http.client`, injects propagation headers; resource `"<METHOD> <host>"` |
| `arq` | see below | `arq.enqueue` (producer) / `arq.job` (consumer) |
| `logging` | `logging.Filter` adding `trace_id`/`span_id` to records; `JSONFormatter` (M4) emits them | — |

**arq propagation (the key exercise).** arq passes `enqueue_job(fn, *args, **kwargs)`
kwargs to the job function, so:

- `inject_job_kwargs(kwargs) -> kwargs` adds `_ozymandias={trace_id,parent_id,sampling_priority}`
  inside an `arq.enqueue` span (type `queue`, `span.kind=producer`).
- `@traced_job` decorator (or `traced(func)` wrapper for `WorkerSettings.functions`
  entries, preserving arq `func(...)` options and `__name__`) pops `_ozymandias`
  from kwargs, `extract`s it, starts `arq.job` as a child (type `worker`,
  resource = function name, `span.kind=consumer`), tags `job.id`, `job.try`,
  and sets metric `queue.wait_ms` from `ctx["enqueue_time"]`. Exceptions mark
  the span and re-raise. Cron jobs (no carrier) start new root traces.

**Node (`sdk/node/src/integrations/`)**

| Integration | Mechanism | Span |
|---|---|---|
| `next` | generic `withTelemetry(handler, {route?})` wrapper for any App Router route handler, plus the lower-level `traceRoute(req, routeHint?, fn)` it is built on (app-node calls the latter from its `handle()`/`authed()` — nothing in the SDK knows that); extracts headers; path normalizer → resource `"GET /api/comics/:id"` | `http.request` |
| `sqlite` | `instrumentSqlite(db)` wraps `Database.prototype.prepare` so returned statements' `run/get/all/iterate` are timed (better-sqlite3 is synchronous — span is created and finished around the call; only when a trace is active, to avoid orphan roots) | `sqlite.query`, resource = `stmt.source` |
| `fetch` | wraps `globalThis.fetch` once (idempotent, keeps Next's own patch chain intact by wrapping whatever is there at `init`); injects headers only for allow-listed hosts (default none — don't leak trace headers to Metron) | `http.client` |
| `winston` | `traceFormat()` winston format adding `trace_id`/`span_id` | — |

**Why no monkey-patching of Next internals:** Next bundles server code, so
`require`-hook patching (how dd-trace works, via `require-in-the-middle` /
`import-in-the-middle`) never sees most modules. app-node's single-door
`authed()` makes explicit wrapping cheap. Write this up in the notes — hitting
this wall is part of the lesson. The SDK package must be listed in app-node's
`serverExternalPackages` so exactly one instance loads; state also lives on
`globalThis[Symbol.for("ozy")]` as a second line of defence.

**Path normalizer (both SDKs):** replace segments that are all digits, UUIDs,
nanoid-like (≥ 16 chars of `[A-Za-z0-9_-]` containing a digit), or hex ≥ 12
with `:id`; cap at 8 segments. Overridable via `routeHint`.

## 3. Agent trace pipeline

`internal/agent/tracerecv` → `concentrator` → `sampler` → forwarder.

1. **Receiver** `POST :8126/v1/traces`: decode, per-span normalize (default
   service, truncate resource/meta values, clamp future/ancient `start`,
   drop invalid ids + count), add `env`/`host` if missing. Body limit 10 MiB;
   semaphore of 8 concurrent decodes → `429` beyond that (SDKs just drop).
2. **Stats concentrator** (on 100% of spans, *before* sampling): for spans with
   `_top_level=1` or `_measured=1`, 10s buckets keyed by
   `(service, name, resource, type, http.status_code class, env)` → hits,
   errors, duration DDSketch. Flushed as series `trace.<name>.hits`,
   `trace.<name>.errors` (counts) and distribution `trace.<name>.duration`
   (seconds), tags `service, resource, env, status_class`. Resource
   cardinality cap per service (default 500; overflow → `resource:_other_`).
3. **Samplers** (a chunk is kept if any says keep):
   priority sampler (`_sampling_priority >= 1`); **error sampler** (any span
   `error=1`, token bucket 10 traces/s); **rare sampler** (first trace seen per
   `(service, name, resource)` per 5 min, token bucket 5/s). Priority rates
   are fed back in the response: target `max_traces_per_second` (default 50)
   per service → `rate = min(1, target / observed)`.
4. Kept spans → batches ≤ 5000 spans / 2 MiB → `POST /v1/traces`.

## 4. TraceStore — `internal/tracestore` (Pebble)

```go
type TraceStore interface {
    Append(ctx context.Context, spans []wire.Span) error
    Trace(ctx context.Context, traceID string) ([]wire.Span, error)
    Search(ctx context.Context, f TraceFilter, fromUs, toUs int64, limit int, cursor string) (*TraceSearchResult, error)
    ServiceEdges(ctx context.Context, fromUs, toUs int64) ([]Edge, error)
    Close() error
}
```

Key design (all integers big-endian; `~ts` = bitwise-inverted start so
iteration is newest-first) — full spec in `docs/formats/tracestore-keys.md`:

| Key | Value | Purpose |
|---|---|---|
| `s/<trace_id 16B>/<span_id 8B>` | zstd(JSON span) | fetch a trace = prefix scan |
| `e/<env>/<service>/<~ts 8B>/<trace_id>/<span_id>` | summary `{name, resource, duration, error, status_code}` — written for `_top_level` spans only | search by service + time; filter resource/duration/error in the scan |
| `r/<env>/<service>/<resource_hash 8B>/<~ts>/<trace_id>/<span_id>` | empty | search narrowed to one resource |
| `x/<env>/<service>/<~ts>/<trace_id>/<span_id>` | empty — error spans only | "errors only" without scanning successes |
| `g/<hour 4B>/<parent_service>/<child_service>` | merge-operator counter `{calls, errors, dur_sum}` | service map edges |
| `t/<hour 4B>/<trace_id>` | empty | retention sweep: list traces by first-seen hour |

Edges: when appending a `_top_level` span with a `parent_id`, look up the
parent span (`s/` point read; it may not have arrived yet — then park the
child id in a small in-memory pending map with a 60s TTL and resolve when the
parent lands; unresolved pendings are counted, not fatal).
Retention (default 7d): sweeper walks `t/` for expired hours and deletes each
trace's `s/`, index and `t/` keys in batches.

## 5. API

- `GET /api/v1/traces?env=&service=&resource=&name=&error=true&min_duration_ms=&max_duration_ms=&status_code=&from=&to=&limit=&cursor=`
  → list of entry-span summaries.
- `GET /api/v1/traces/{trace_id}` → `{spans:[…], services:[…], duration, span_count, errors}` (sorted by start; orphans attached under a synthetic "missing parent" node flag).
- `GET /api/v1/services?env=&from=&to=` → per service: type, req/s, error %, p50/p95/p99 — computed from `trace.*` metrics via metricql (not from stored spans: sampled data would lie).
- `GET /api/v1/services/{service}/resources` → same per resource.
- `GET /api/v1/service-map?env=&from=&to=` → nodes + edges.
- Logs API gains nothing new: `trace_id:<id>` already works as a reserved logql key.

## 6. UI — APM
- **Service list** (RED table, sparkline per row) → **Service page**
  (requests/errors/latency timeseries, latency distribution histogram drawn
  from the DDSketch bins, resource table, recent error traces).
- **Trace search** (filters as above, duration scatter plot over time).
- **Trace view:** flame graph + waterfall toggle (canvas-rendered, colour by
  service, zoom/pan, collapse subtrees, critical-path highlight), span detail
  panel (tags, error stack, SQL text), **Logs tab** (`trace_id:<id>` query,
  inline), "queue wait" rendered as a distinct hatched gap between
  `arq.enqueue` and `arq.job`.
- **Service map:** force-directed graph, edge width = call rate, red tint = error rate.
- Log Explorer's `trace_id` link now opens the trace view; metrics widgets get
  "view related traces" (same service + time window).

## 7. Integrations
Per `docs/private/integrations.md` (M5 parts) for both apps.

## 8. Test plan

- **L1** (both SDKs) span lifecycle, idempotent finish, error capture +
  re-raise unchanged, nested parenting, context isolation across concurrent
  async tasks (100 interleaved tasks never cross-parent), thread/executor
  behaviour, trace buffer flush on root finish / partial flush / late chunk,
  deterministic sampling agreement across SDKs (shared vector file: trace_id
  × rate → decision, identical in Python, Node and Go), `rate_by_service`
  uptake, writer bounds/drop-oldest, path normalizer table (shared vectors).
  Each integration against the real library in tests: FastAPI `TestClient`
  (route pattern incl. mounted routers, 404, exception, streaming),
  SQLAlchemy on SQLite (statement text, no params ever present — assert by
  scanning the emitted payload for a sentinel parameter value), fakeredis-free
  redis wrapper test via a stub connection, httpx `MockTransport` (headers
  injected), arq (`inject_job_kwargs` → `traced` job: same trace id, correct
  parent, `_ozymandias` kwarg never reaches the wrapped function, `func()`
  options preserved, cron root trace), logging filter. Node: `traceRoute`,
  `instrumentSqlite` on an in-memory better-sqlite3 (incl. statement that
  throws), fetch wrapper (allow-list, idempotent double-wrap, preserves
  Response/abort semantics), winston format.
  (Go) receiver normalization; concentrator bucket math + top-level rule +
  cardinality cap; each sampler with fake clock; rate feedback; key
  encoders (ordering properties); search filters; edge resolution incl.
  child-before-parent arrival and TTL expiry; retention sweep.
- **L2** `extract(inject(ctx)) == ctx` for headers and job kwargs; key
  encoding: byte order == (newest-first time order); concentrator: Σ hits over
  buckets == number of top-level spans in; sampled ⊆ received.
- **L4** fuzz: `wire.DecodeTraces`, propagation header parsing (garbage never
  throws, yields "no context"), trace key decoders.
- **L5** agent down / slow / 429 / 500 → app latency unaffected (assert
  wrapped call overhead), bounded memory, drops counted; ozyd crash loop
  with Pebble → acknowledged spans present.
- **L6** concentrator + store under `-race` with concurrent appends/searches;
  Python: 1000 concurrent asyncio tasks tracing; Node: 1000 concurrent ALS scopes.
- **L7** wire goldens hops B and F; sampling + normalizer shared vectors; API goldens.
- **L8** **cross-process propagation test:** in one pytest, a FastAPI app with
  the middleware enqueues via the arq integration into a real arq worker run
  in-process against a Redis test container (marked like app-python's
  `docker` marker) — assert one trace id, parent chain
  `http.request → arq.enqueue → arq.job → postgres.query`.
- **L9** smoke v5: POST a canned 3-span cross-service trace to the agent →
  `/api/v1/traces/{id}` returns 3 spans; `trace.*.hits` metric appears;
  service map has the edge.
- **L11** flame-graph layout (pure function: spans → rects; overlapping
  siblings, orphan spans, clock-skewed children clamp) unit tests; Playwright:
  search → open trace → span detail → logs tab.
- **L12** tracer overhead per span (time + allocations) in both SDKs, measured
  with tracing disabled / enabled-unsampled / enabled-sampled; no budget is
  set in advance — measure, report, and record the number in the notes so
  regressions are visible. Also agent spans/sec and store append + search latency.

## 9. Docs deliverables
`docs/sdk/*.md` tracing + every integration + "writing your own integration";
`docs/formats/tracestore-keys.md`; `docs/api.md` APM endpoints; DESIGN.md:
span model, context propagation (in-process and cross-process, with the arq
hop), stats-before-sampling argument, sampler design, key design rationale,
service-map derivation; diagrams `trace-path.mmd`,
`arq-propagation-sequence.mmd`; ADRs: own headers vs W3C, microsecond
timestamps, explicit wrapping in Next.js, head-sampling-in-SDK +
error/rare-in-agent; app-python `docs/submission-flow.mmd` updated with the
trace-context hop; `docs/notes/M5.md` incl. the Next.js bundling write-up.

## 10. Acceptance criteria

- [ ] Submitting a solution in app-python yields **one** trace spanning api and worker: `http.request POST /api/v1/submissions` → `postgres.query`s → `arq.enqueue` → (queue wait gap) → `arq.job judge_submission` → `judge.container.run` → `postgres.query` (verdict write).
- [ ] app-node traces show `http.request` → `sqlite.query` children, plus `http.client` to Metron and image-processing spans on the relevant routes, with `:id`-normalized resources.
- [ ] From a log line → its trace → back to that trace's logs, in both apps.
- [ ] Service page RED numbers match the M3 request metrics within 1% over the same window, with sampling forced to 10% (proves stats-before-sampling).
- [ ] With `OZY_TRACE_SAMPLE_RATE=0.1`, error traces and first-seen resources are still kept.
- [ ] No SQL parameter values, Redis keys, cookies or Authorization headers appear in any stored span (explicit scan test).
- [ ] Measured tracer overhead reported; both apps' test suites green with ozymandias absent.
- [ ] Tests and docs deliverables complete; `docs/notes/M5.md` has evidence.
