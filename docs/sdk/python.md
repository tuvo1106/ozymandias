# Python SDK

`ozy` sends metrics from any Python 3.12+ application to an ozymandias agent. It has no
runtime dependencies, it never raises into your code, and it does nothing at all until you
point it at an agent.

The SDK ships the statsd client, an ASGI metrics middleware ([below](#asgi-middleware)) and,
from M5, a tracer with integrations for ASGI, SQLAlchemy, Redis, httpx, arq and `logging`
([Tracing](#tracing)).

The SDK is a convenience layer. The public interface is the wire protocol
([`docs/wire-protocol.md`](../wire-protocol.md) §A), so any extended StatsD client can talk to the
agent. The SDK exists to get the fiddly parts right once: formatting, buffering, sampling,
fork safety and failure isolation.

## Install

The package is not on PyPI yet — the SDK is pre-1.0 (ADR-0014). The name `ozy`
is free there and will be claimed at the first publish
([extensibility.md](../plan/extensibility.md) §5). There are three ways to install it:

| From | Command |
|---|---|
| PyPI (future) | `pip install ozy` / `uv add ozy` |
| git | `pip install "git+https://github.com/tuvo1106/ozymandias#subdirectory=sdk/python"` |
| a built wheel | `cd sdk/python && uv build`, then `pip install dist/ozy-0.2.0-py3-none-any.whl` |

For the git URL, you need read access to the repository. For vendoring into an app, copy the
wheel into the app and install it from a path. `make sdk-release` automates this (M1).

## Quick start

```python
import ozy
from ozy import statsd

ozy.init(service="shop", env="dev", version="1.2.0")

statsd.increment("checkout.completed", tags=["payment:card"])
statsd.gauge("queue.depth", 3)
statsd.distribution("http.request.duration", 12.4, tags=["route:/api/items"])
```

Then run the app with `OZY_AGENT_HOST=127.0.0.1`. Without it, nothing is sent (see
[Safety guarantees](#safety-guarantees)).

## Configuration

Settings come from `ozy.init(...)` arguments, with the environment underneath:
**argument > environment variable > default**. An argument left as `None` falls through to
the environment. This lets an operator point an unmodified app at an agent, while the app
still pins the values it owns, such as its service name.

| `init()` argument | Environment variable | Default | Effect |
|---|---|---|---|
| `agent_host` | `OZY_AGENT_HOST` | unset | Agent host or IP. **Unset disables the SDK entirely.** `agent_host=""` disables it even when the env var is set |
| `statsd_port` | `OZY_STATSD_PORT` | `8125` | Agent UDP port. An invalid env value falls back to 8125 |
| `service` | `OZY_SERVICE` | unset | Adds `service:<value>` to every metric |
| `env` | `OZY_ENV` | unset | Adds `env:<value>` |
| `version` | `OZY_VERSION` | unset | Adds `version:<value>` |
| `tags` | `OZY_TAGS` | none | Extra tags on every metric. The env var is a comma list: `team:core,region:eu` |
| `debug` | `OZY_DEBUG` | off | `1`/`true`/`yes`/`on`: send failures go to the `ozy` logger at WARNING and payloads at DEBUG |
| `max_payload` | none | `1432` | Maximum datagram size in bytes (one Ethernet MTU minus headers). The agent reads at most 8192 |
| `flush_interval` | none | `0.1` | Seconds between background flushes |
| `trace_enabled` | `OZY_TRACE_ENABLED` | on | Whether spans are recorded **when an agent host is set**. `0`/`false`/`no`/`off` switches tracing off and leaves metrics on |
| `trace_port` | `OZY_TRACE_PORT` | `8126` | Agent TCP port for `POST /v1/traces`. An invalid value falls back to 8126 |
| `trace_sample_rate` | `OZY_TRACE_SAMPLE_RATE` | `1.0` | Head-sampling rate, clamped to [0, 1]; the agent's `rate_by_service` answer overrides it per service ([Sampling](#sampling)) |
| `integrations` | none | none | Names or [`Integration`](#writing-your-own-integration) objects to patch (`"sqlalchemy"`, `"redis"`, `"httpx"`, `"arq"`, `"logging"`, `"asgi"`). Applied only when tracing is enabled |

Calling `init()` again flushes and closes the previous configuration, then applies the new
one. `init()` never raises. If configuration fails, it logs a warning and leaves the SDK
disabled.

## API reference

`from ozy import statsd` gives you the process-wide client. It exists from import time,
so any module can import it before or after `init()`. `init()` reconfigures that same object.

The metric methods take `(name, value, tags=None, sample_rate=1.0)`. `tags` is a list of
`"key:value"` or bare `"key"` strings. A single string is treated as one tag.

| Method | Wire type | Agent aggregation per 10 s | Use for |
|---|---|---|---|
| `increment(name, value=1, …)` | `c` | sum of value/rate | events: requests, errors, jobs |
| `decrement(name, value=1, …)` | `c` (negated) | same | the same counter counting down |
| `gauge(name, value, …)` | `g` | last value | current levels: queue depth, pool size |
| `histogram(name, value, …)` | `h` | avg, min, max, median, p95 and count per host | per-host value spreads |
| `distribution(name, value, …)` | `d` | a DDSketch from M2 (like `h` in M1) | latencies you want as global percentiles |
| `timing(name, ms, …)` | `ms` | like `h` | durations in milliseconds |
| `set(name, member, …)` | `s` | count of distinct members | unique users, unique ids |

Prefer `distribution` over `histogram` for latencies. An average of per-host p95s is not a
p95, while sketches merge exactly.

### `statsd.timed(name, tags=None, sample_rate=1.0)`

This times a block or a function and records the elapsed milliseconds with `timing`. It
works in all of these forms:

```python
with statsd.timed("job.duration", tags=["queue:default"]):
    run_job()

async with statsd.timed("fetch.duration"):
    await fetch()

@statsd.timed("render.duration")
def render(): ...

@statsd.timed("fetch.duration")
async def fetch(): ...   # detected with inspect.iscoroutinefunction; awaited inside
```

The duration is recorded even when the code raises. The exception itself propagates
unchanged: the SDK never wraps or swallows it. Each call of a decorated function is timed on
its own, so concurrent and recursive calls are fine. Don't share one `timed(...)` object as
a context manager across threads. Create a new one per block.

### `statsd.flush()`, `statsd.close()`, `statsd.stats()`

- `flush()` sends everything that's buffered, on the calling thread. You rarely need it,
  because the background flusher runs every 100 ms. It's useful at the end of a short job,
  before `os._exit()`, and in tests.
- `close()` flushes, stops the flusher, closes the socket and disables the client until the
  next `init()`. When the SDK is enabled, `close()` is registered with `atexit`.
- `stats()` returns `Stats(sent, packets, dropped, errors)`, the same shape as the Node SDK's
  `stats()`. The counts start at `init()` (or at `fork()` in a child process):
  - `sent`: messages in datagrams the kernel accepted. Accepted doesn't mean delivered,
    because UDP gives no receipts.
  - `packets`: datagrams the kernel accepted. `sent / packets` is how many messages were
    coalesced into each datagram on average.
  - `dropped`: messages that never reached the kernel. That covers NaN or ±Inf values,
    non-numbers, payloads evicted from the full queue, and payloads whose send failed.
    Sampled-out calls aren't counted.
  - `errors`: DNS failures, socket errors, and internal errors that the no-raise wrapper
    swallowed.

`StatsdClient` is exported too, for tests and for sending to a second agent. Its constructor
accepts injected `random`, `clock`, `resolver` and `socket_factory` callables, and
`configure(ozy.Config(...))` enables it.

## Formatting and sampling

The exact bytes are specified in [wire-protocol.md](../wire-protocol.md) §A ("What a client
sends"). They're pinned by
[`sdk-cases.json`](../../pkg/wire/testdata/statsd/sdk-cases.json), which this SDK's
tests, the Node SDK's tests and the Go parser all load.

```
<name>:<value>|<type>[|@<rate>][|#<call tags>,<init tags>,service:…,env:…,version:…]
```

- **Numbers.** Integral values print without a decimal point (`3`, not `3.0`). Anything else
  prints as the shortest string that round-trips a float64, which is Python's `repr`, for
  example `12.4` or `0.30000000000000004`. NaN and ±Inf are dropped (and counted) instead of
  sent, because the agent would reject the line anyway. Very large or very small magnitudes
  keep `repr`'s exponent form (`1e+16`, `1e-07`), which the agent parses. The goldens don't
  pin those forms, so they may differ from the Node SDK byte for byte.
- **Tags.** The call's tags come first, then the `init()` / `OZY_TAGS` tags, then
  `service:`, `env:` and `version:` for whichever of those are set.
- **Sanitation.** This is deliberately minimal. `|`, `,` and newline become `_` in names,
  tags and set members, and so does `:` in names. These are the only characters that could
  break the framing of a datagram. Lowercasing, length limits and character sets are left to
  the agent, so a single normalizer is the authority.
- **Sampling.** With `sample_rate < 1`, a message is sent only when `random() < rate`, and
  it carries `|@rate`. The agent scales counts by `1/rate`, so sampled counters still
  estimate the true total. `|@1` is never written.

## Buffering

Messages are joined with `\n` into one buffer. A datagram is handed off when:

1. the next message would push it past `max_payload` (1432 bytes, measured in UTF-8 bytes),
   in which case the full buffer moves to a send queue and the flusher wakes immediately;
2. `flush_interval` (100 ms) passes, in which case the flusher sends whatever is buffered;
3. `flush()` or `close()` is called.

A single message larger than `max_payload` still goes out, alone in its own datagram.

Your code's thread only formats a string and appends it under a lock. It never does I/O.
All sends happen on one **daemon thread** named `ozy-statsd-flusher`. The thread starts
on the first metric, not at `init()`, and it never keeps the interpreter alive.

The send queue holds at most 64 full payloads (about 90 KiB). If the agent is unreachable
and the queue fills, the **oldest** payload is dropped and counted. Memory stays bounded no
matter how long the agent is gone.

## Safety guarantees

These are tested in `sdk/python/tests/test_safety.py` (the L10 safety suite in
[testing.md](../plan/testing.md)):

- **No agent host, no footprint.** With `OZY_AGENT_HOST` unset, every call is a no-op.
  No socket, no thread, no `atexit` hook and no fork hook is ever created. It's safe to leave
  instrumentation in code that runs in unit tests and CI.
- **Nothing raises into your app.** Every public method is wrapped, and any `Exception`
  becomes an `errors` increment. Unreachable ports, DNS failures, full socket buffers and bad
  values are all counted, never raised. `KeyboardInterrupt` and `SystemExit` still propagate.
- **Calls don't block.** A call only enqueues, even while the flusher is stuck.
- **Your exceptions are yours.** Code inside `timed` raises exactly what it raised.
- **Bounded memory**, as described in [Buffering](#buffering).

## ASGI middleware

`ozy.integrations.asgi.MetricsMiddleware` records one count and one duration per HTTP
request, for a Starlette-family app (Starlette, FastAPI):

```python
from ozy.integrations.asgi import MetricsMiddleware

app = FastAPI()
app.add_middleware(MetricsMiddleware, exclude_paths=["/healthz"])
```

Other ASGI frameworks (Litestar, Django-ASGI) never set `scope["route"]`, so every request there
is `route:unmatched`: the counts, errors and durations are right, the per-route breakdown is not.

Add it last, so it is the outermost middleware you have added and times the others. Starlette's
own error handler still sits outside it, which is why a handler that raises is recorded here as
`500`: this middleware sees the exception before the handler turns it into a response.

| Metric | Type | Tags |
|---|---|---|
| `http.request.count` | counter | `route`, `method`, `status`, `status_class` |
| `http.request.duration` | distribution, ms | the same |

- **`route` is the route pattern**, such as `/problems/{slug}`, read from
  `scope["route"]` after the inner app returns (routing has not happened when the request
  arrives). It is the path *as the framework reports it*: FastAPI's `include_router(prefix=...)`
  is not part of it, so an app mounted under `/api/v1` sees `/problems/{slug}`, not
  `/api/v1/problems/{slug}`. A sub-app reached through Starlette's `Mount` is similar: depending
  on the Starlette version its route is relative to the mount, or absent and so `unmatched`. Neither
  can be recovered from inside a middleware; to get exact patterns for a mounted app, add the
  middleware inside it. A request that matched nothing is `route:unmatched`, so a scanner probing
  random URLs is one series rather than thousands.
- **`exclude_paths` matches the raw request path exactly**, as the server reports it in
  `scope["path"]`. That does include a router prefix, but `/healthz` does not exclude
  `/healthz/`, and behind a proxy that strips or adds a prefix the right value depends on
  the server.
- **`method` is one of the standard verbs, or `OTHER`.** A scanner can send any token as a
  method, and an unbounded tag is one series per value.
- **Status is honest.** An app that raises before answering is recorded as `500`, since that
  is what the server will send. A request cancelled before any response started is `499` only
  when the client's disconnect was seen, so closing a tab does not look like a server fault.
  A cancellation with no disconnect behind it (a shutdown, a reload, a timeout scope outside
  this middleware) is the server's doing and is `500`. Even with a disconnect seen, only a
  failure that looks like a closed connection (a cancellation, an `OSError`, Starlette's
  `ClientDisconnect`) is `499`: a handler bug that follows one stays `500` so it reaches your
  5xx alerts. A failed write of the response's first message counts as no response, not as the
  status the app tried to send. Once a response has started, its status stands even if the body then fails.
- **Duration runs until the inner app returns**, so a streamed response, server-sent events or a
  long poll records its whole lifetime as latency. That is the honest "how long did this request
  take", but it is not time-to-first-byte; list such routes in `exclude_paths` if their numbers
  would swamp a latency chart. A bare string for `exclude_paths` is treated as one path.
- **WebSockets and lifespan events pass through unrecorded.** Duration for a socket that
  lives for an hour would mean nothing.
- **It is pure ASGI**, not Starlette's `BaseHTTPMiddleware`, so streaming responses,
  background tasks and context variables behave as they do without it.
- **It never raises into a request.** A failure while recording is logged at debug level
  and dropped. Until `ozy.init()` enables the client it is a straight passthrough.

## Tracing

`from ozy import tracer`. A **span** is one timed unit of work with an id, a parent id and the id
of the **trace** it belongs to; a trace is the tree of spans one request caused, across every
process it touched. Spans are sent to the agent (`POST :8126/v1/traces`, wire-protocol §B), which
computes request/error/latency statistics from them, samples them and forwards the kept ones to the
trace store.

```python
import ozy
from ozy import tracer

ozy.init(service="api", env="dev", integrations=["sqlalchemy", "redis", "httpx", "arq", "logging"])

with tracer.trace("judge.run", resource=language, type="worker") as span:
    span.set_tag("problem.id", problem_id)
    span.set_metric("tests.count", n)

@tracer.wrap("cover.resize")        # sync or async; bare @tracer.wrap names it after the function
async def resize(...): ...
```

Without an agent host (or with `OZY_TRACE_ENABLED=0`) the tracer is inert: `trace()` returns one
shared no-op span, your code still runs, and no thread, socket or exit hook is created.

### API

| Call | What it does |
|---|---|
| `tracer.trace(name, *, resource=None, service=None, type="custom", tags=None, child_of=None)` | Start an **active** span; use it as `with` or `async with`. Leaving the block finishes it. |
| `tracer.wrap(name=None, *, resource, service, type, tags)` | Decorator, sync and async. A generator function is traced only while it is created, not across its yields. |
| `tracer.start_span(name, ..., child_of=None, activate=True)` | The manual API: you call `span.finish()`. `activate=False` for a leaf that needs no children. |
| `tracer.current_span()` | The active span of this task or thread, or `None`. |
| `tracer.current_trace_context()` | The active span's `Context(trace_id, span_id, sampling_priority)`, or `None`. |
| `tracer.inject(carrier, context=None)` | Write the three `x-ozy-*` propagation headers into a mutable mapping. |
| `tracer.extract(carrier)` | Read a `Context` from headers (any case, `str` or `bytes`) or from a job carrier dict; `None` for absent or malformed. |
| `tracer.wrap_executor(executor)` | An executor whose tasks run in the submitter's context (see below). |
| `tracer.stats()` | `TracerStats(spans_started, spans_finished, chunks_sent, chunks_dropped, send_errors)`. |
| `tracer.flush()` / `tracer.close()` | Send everything queued now, on the calling thread / drain and disable. Rarely needed: the writer flushes every second and on exit. |
| `ozy.normalize_path(path)` | The path normalizer ([below](#resources-and-the-path-normalizer)). |

A span has `set_tag(key, value)` (strings; `None` is ignored, other values are stringified),
`set_metric(key, number)` (finite numbers only), `set_error(exc=None)`, `finish()` and the read-only
`trace_id`, `span_id`, `parent_id`, `context`. Everything is safe to call on a finished span; nothing
raises.

**Lifecycle.** Ids are random (`os.urandom`; 128-bit trace, 64-bit span, lowercase hex, never zero).
`start` is wall-clock microseconds; `duration` comes from the monotonic clock, so a clock step during
a span cannot make it negative. An exception leaving a `with` block sets `error=1` and
`error.type` / `error.message` / `error.stack`, then **propagates unchanged**: the same object, not a
wrapper. A `CancelledError` ends the span but is not an error. `finish()` is idempotent.

**Context.** The active span lives in a `contextvars.ContextVar`, so it survives `await`, is copied
into `asyncio.create_task` (a span opened inside the task cannot disturb its spawner), and is **not**
copied into plain threads: a new thread starts a new trace. `asyncio.to_thread` copies the context
for you; for a `ThreadPoolExecutor` use `tracer.wrap_executor(pool)`. A span finished from a different
context than the one that started it does not corrupt the stack (the SDK compares against the active
span instead of using `ContextVar.reset(token)`, which raises there).

### The trace buffer

Spans are not sent one by one. The finished spans of a trace accumulate in a buffer held by the
**local root**, the first span this process saw for the trace. When the local root finishes the buffer
goes to the writer as one **chunk**, which is what lets the agent judge a whole trace (error and
rare-trace sampling look at every span of a chunk). A trace holding more than 500 finished spans
flushes a *partial* chunk; a span that finishes after its root (a detached task) is sent as a *late*
chunk of its own. Partial and late chunks carry `_sampling_priority` on their first span, because
there is no root in them to carry it. Unfinished spans are never sent.

`_top_level=1` is set on a span whose parent is absent or belongs to another service (including a
span continued from another process): the agent counts these, and only these, as service entry
spans in its request statistics.

### Sampling

The head-sampling decision is made once, at the trace's first span, as a pure function of the trace
id: `keep == (low64(trace_id) * 1111111111111111111 mod 2**64) < rate * 2**64`. Every service in a
trace, in any language, computes the same answer without talking to the others, and a downstream
service **inherits** the upstream priority rather than re-deciding. The rate is
`OZY_TRACE_SAMPLE_RATE`, then overridden per service by the agent's `rate_by_service` response
(key `service:<name>,env:<env>`); the shared vectors in `pkg/wire/testdata/traces/sampling.json` pin
the function.

**A dropped trace is still sent**, with `_sampling_priority = 0`. The agent computes its statistics
over every span *before* it samples, so a 10% sample must not show up as 10% of the traffic; the drop
happens in the agent. This is why sampled and unsampled traces cost the same in the SDK
([overhead](#overhead)).

### Propagation

Between services, three headers (wire-protocol §B):

```
x-ozy-trace-id: <32 hex>    x-ozy-parent-id: <16 hex>    x-ozy-sampling-priority: <-1|0|1|2>
```

`tracer.inject(headers)` writes them for the active span; `tracer.extract(headers)` reads them back.
A header that is malformed in any way (wrong length, zero id, a priority outside -1..2) yields **no
context at all**, and the receiver starts a fresh trace instead of continuing a corrupt one. Uppercase
hex is accepted and lowered. `extract(inject(ctx)) == ctx` for every valid context; garbage never
raises.

### Resources and the path normalizer

An HTTP span's resource is `"<METHOD> <route pattern>"` when the framework knows the pattern. When it
does not (a 404, a framework that never sets `scope["route"]`), `normalize_path()` produces a
low-cardinality guess: a segment that is all digits, a UUID, hex of 12 or more characters, or
nanoid-like (16 or more of `[A-Za-z0-9_-]` containing a digit) becomes `:id`, the query string and
fragment are dropped, and at most 8 segments are kept. Vectors:
`pkg/wire/testdata/traces/normalize-path.json`, shared with Go.

### Limits the SDK applies

So that it never sends a span the agent would normalize or refuse: `resource` and each `meta` value
are cut to 5000 bytes on a character boundary, a span keeps at most 100 `meta` and 50 `metrics`
entries (extras dropped in sorted key order; `_sampling_priority` and `_top_level` always survive),
`name` and `service` are cut to 100 bytes, an unknown `type` becomes `custom`, and a non-finite metric
is dropped. Queued chunks are bounded (1000; the **oldest** is dropped and counted in
`stats().chunks_dropped`). The writer is a daemon thread that posts every second or at 100 queued
chunks, with a 2 s timeout and **no retries** (traces are best-effort; a `429` means "agent busy" and
the chunk is dropped). It drains on exit within a 1 s budget, and restarts after `fork()` like the
statsd flusher ([Fork behaviour](#fork-behaviour)). `rate_by_service` from a `200` response is applied
to later decisions; garbage in it is ignored.

### Integrations

`ozy.init(integrations=[...])` patches the named integrations (only when tracing is enabled);
`ozy.integrations.patch_all()` patches every one whose library is installed; an unavailable one is a
silent no-op and a failing one is logged and skipped. Each imports its target lazily, is idempotent,
and has an `unpatch()`. **The client integrations (SQLAlchemy, Redis, httpx, arq's enqueue) only record
inside an active trace**: a query with no active span (a migration, a startup probe) would otherwise
become a root span, and a root span counts as a service entry in the agent's request statistics.

| Integration | Mechanism | Span |
|---|---|---|
| `asgi` | `TraceMiddleware(app)`, pure ASGI; or `patch()` to wrap `build_middleware_stack` of Starlette/FastAPI apps built afterwards | `http.request`, type `web`, resource `"<METHOD> <route pattern>"`, `http.method`, `http.url` (path only), `http.route`, `http.status_code`, `span.kind=server` |
| `sqlalchemy` | event listeners (`before_cursor_execute`, `after_cursor_execute`, `handle_error`) on the `Engine` class (`patch()`) or one engine (`instrument(engine)`, also `AsyncEngine`) | `<dialect>.query` (`postgres.query`, `sqlite.query`), type `db`, resource = statement text cut to 2000 chars, `db.rowcount` |
| `redis` | wraps `execute_command` and pipeline `execute` on `redis` and `redis.asyncio` | `redis.command`, type `cache`, resource = command **name** only; a pipeline is one span, resource `PIPELINE`, metric `redis.pipeline.commands` |
| `httpx` | wraps `Client.send` / `AsyncClient.send`, injects the headers | `http.client`, type `http`, resource `"<METHOD> <host>"`, `http.status_code`; a 5xx marks it as an error |
| `arq` | `inject_job_kwargs` / `traced` ([below](#arq-the-trace-crosses-the-queue)); `patch()` wraps `ArqRedis.enqueue_job` | `arq.enqueue` (queue, producer) and `arq.job` (worker, consumer) |
| `logging` | a record factory (or `TraceLogFilter` on a handler) stamping `trace_id` / `span_id` | none |

**ASGI.** `TraceMiddleware` extends [`MetricsMiddleware`](#asgi-middleware): one middleware, one
set of facts, so the span and the `http.request.count` metric cannot disagree about the route or the
status. Use it instead of `MetricsMiddleware`, not beside it. The span is started before the inner app
runs (so spans made while handling the request are its children) and finished after, when the route
pattern is known. A 5xx or an exception marks it as an error; a 4xx and a client disconnect (499) do
not. WebSockets and lifespan pass through unrecorded. With FastAPI 0.142 / Starlette 1.7 an
`include_router(prefix=...)` prefix **is** part of the pattern the span reports (checked in
`tests/test_trace_asgi.py`); the version-dependent behaviour of `Mount`ed sub-apps described under
[ASGI middleware](#asgi-middleware) still applies, and the span falls back to the normalized path.
`patch()` leaves an app alone that already added `MetricsMiddleware` or `TraceMiddleware`, so a
request is never counted twice.

**SQLAlchemy.** The statement text is the resource; **parameters are never read**, so no bound value
(an email, a token) can reach a span. A driver's error message can quote a value (PostgreSQL's
`DETAIL: Key (email)=(a@b.c) already exists`), so a failed statement records the exception type and
only the **first line** of its message, and no stack. The one thing this cannot protect is a statement
an application built by pasting values into the SQL string itself.

**Redis.** Only the command name is recorded: keys and values are user data often enough
(`session:<token>`) that "keys are fine" is a leak waiting to happen. A failure records the exception
type only, since Redis errors quote arguments.

**httpx.** Headers are injected for **no** host by default (the Node `fetch` integration does the same):
a trace id and a sampling decision are a correlation handle and a lever a third party should not hold
or set. `ozy.integrations.httpx.INTEGRATION.inject_hosts = {"payments.internal"}` names the services of
yours that should join the trace (the span is recorded either way). The `http.url` tag has no query string and no
credentials.

#### arq: the trace crosses the queue

arq pickles `enqueue_job(function, *args, **kwargs)` into Redis and a worker, in another process, calls
`function(ctx, *args, **kwargs)` later. The trace context therefore rides in a reserved kwarg,
`_ozymandias={"trace_id", "parent_id", "sampling_priority"}`, and the two ends agree on it:

```
API process                                       worker process
http.request
  └─ arq.enqueue (producer) ── _ozymandias ──►    arq.job (consumer, queue.wait_ms)
                                                    └─ postgres.query ...
```

```python
from ozy.integrations.arq import inject_job_kwargs, traced

# producer: either patch() (init(integrations=["arq"])) -- every enqueue_job inside a trace -- or
await pool.enqueue_job("judge", **inject_job_kwargs({"submission_id": sid}, function="judge"))

# consumer: wrap the worker's functions; plain coroutines, arq.func(...) and arq.cron(...) all work
class WorkerSettings:
    functions = [traced(judge), traced(arq.func(grade, timeout=30, max_tries=2))]
    cron_jobs = [traced(arq.cron(nightly, hour=3))]
```

`traced` (alias `traced_job`, usable as a decorator) **pops** `_ozymandias` from the kwargs before
calling your function, so it never sees an argument it does not declare; extracts the context; starts
`arq.job` as a child (type `worker`, resource the function name, `span.kind=consumer`); tags `job.id`
and `job.try`; and sets the metric `queue.wait_ms` from `ctx["enqueue_time"]`. The gap between
`arq.enqueue` and `arq.job` is the time the job sat in the queue. An exception marks the span and
re-raises the same object; `arq.Retry` is control flow and does not mark an error; a cancellation ends
the span without one. `arq.func(...)` options and `__name__` are preserved. A cron job has no
enqueuer, so it carries no context and starts a new root trace per run. A legacy job with no kwarg
(from an API that predates the integration) still runs and starts a root trace.

**Deploy workers before the API.** `inject_job_kwargs` and the enqueue `patch()` add a kwarg your job
functions do not declare, and a worker whose functions are not wrapped with `traced` would call
`fn(ctx, _ozymandias={...})` and fail with a `TypeError`. A worker with `traced` and no tracing
configured still strips the kwarg.

`inject_job_kwargs` starts *and finishes* its `arq.enqueue` span in one call, so that span is an
instant: it marks when the job was enqueued and is the job's parent, but it does not time the Redis
write. The `patch()` wraps `enqueue_job` itself and times the real call, which is why it is the better
choice.

`tests/test_trace_arq_e2e.py` runs the whole chain (`http.request` -> `arq.enqueue` -> `arq.job` ->
`sqlite.query`, one trace id, the parent chain asserted) through a real arq worker against a real
Redis; it is skipped unless `OZY_TEST_REDIS_URL` points at a dedicated non-zero database. CI runs it
against a Redis service container (database 15); a laptop run skips it unless you set the variable.

**Logging.** The `logging` integration installs a record factory, so *every* record in the process
carries `trace_id` and `span_id` whichever logger or handler produced it (a filter on a logger would
miss its children's records, since filters do not propagate); `TraceLogFilter` does the same on one
handler. [`JSONFormatter`](#structured-logging) already emits both fields from the record. Outside a
span the fields are simply absent.

### Writing your own integration

An integration is any object with this shape (`ozy.integrations.Integration`, a runtime-checkable
`Protocol`), registered with `register_integration()` and built on **only** the public tracer API, so
a third party has exactly the power the built-ins have:

```python
from ozy import tracer
from ozy.integrations import register_integration

class MyLibIntegration:
    name = "mylib"

    def is_available(self) -> bool:          # no side effects; never raise
        try:
            import mylib  # noqa: F401
        except ImportError:
            return False
        return True

    def patch(self) -> None:                 # idempotent
        import mylib
        original = mylib.Client.call
        def call(self, *args, **kwargs):
            if tracer.current_span() is None:        # client spans: only inside a trace
                return original(self, *args, **kwargs)
            with tracer.trace("mylib.call", type="http", resource=args[0]):
                return original(self, *args, **kwargs)
        mylib.Client.call = call
        self._original = original

    def unpatch(self) -> None:               # idempotent
        import mylib
        mylib.Client.call = self._original

register_integration(MyLibIntegration())     # then init(integrations=["mylib"]) or patch_all()
```

Rules the built-ins follow, and yours should: import the target lazily and no-op when it is absent;
never raise into the host (`patch()`/`patch_all()` log and skip a failing integration, but a wrapper
that raises at call time is yours to guard); never record parameters, keys, credentials or query
strings; start a span only when one is active unless the call is a genuine entry point; and let the
host's exception propagate unchanged. `ozy.integrations._patching.PatchSet` (private; copy it if you
like) shows how to restore an *inherited* method on undo by deleting it rather than copying it onto
the subclass.

### Safety guarantees (tracing)

Each is pinned by a test in `sdk/python/tests/`:

- **Disabled means inert**: no agent host, or `OZY_TRACE_ENABLED=0`, means no thread, socket, `atexit`
  or fork hook (`test_disabled_means_no_thread_no_hooks`), and your wrapped code behaves identically.
- **Nothing raises into the host.** Every tracer entry point and every integration wrapper swallows its
  own failures (tests break the tracer, the id source and the integrations deliberately). The one
  deliberate exception is user code inside `trace()` / `wrap()`, whose exceptions propagate unchanged.
- **A slow or dead agent never slows the app**: `submit` is a lock and a list append
  (`test_a_slow_agent_never_blocks_the_caller`), and memory is bounded by the queue
  (`test_the_queue_is_bounded_and_drops_the_oldest`).
- **No SQL parameter, Redis key/value or query string reaches a span**: each integration's test
  scans the raw bytes it posted for a sentinel value.

### Overhead

`uv run python benchmarks/trace_overhead.py` measures per-span time and allocation with tracing
disabled / enabled-unsampled / enabled-sampled (the writer replaced by a counter, so it measures the
tracer, not the loopback). No budget is set; the numbers are recorded in the M5 notes so a regression
is visible. Measured on an Apple-silicon laptop, CPython 3.12, `--n 50000` (a reference, not a promise):

| | disabled | unsampled | sampled |
|---|---|---|---|
| root span | 0.12 us | 3.6 us | 3.7 us |
| child span (11-span trace) | 0.12 us | 2.5 us | 2.5 us |
| `@wrap` call overhead | 0.16 us | 3.7 us | 3.8 us |
| peak bytes, one root span | 128 | 1174 | 1174 |
| bytes retained per buffered span | 0 | 669 | 669 |

Unsampled costs the same as sampled by design: the trace is still built and sent.

## Structured logging

`ozy.integrations.logging.JSONFormatter` writes each `logging` record as one JSON line, which the
agent's `json` source reads as fields instead of guessing at text:

```python
import logging
from ozy.integrations.logging import JSONFormatter

handler = logging.StreamHandler()          # stdout/stderr of a container, or a FileHandler
handler.setFormatter(JSONFormatter())
logging.getLogger().addHandler(handler)

logging.getLogger("shop").info("order placed", extra={"order_id": 42, "total": 19.5})
```

```json
{"timestamp":"2026-10-04T12:00:00.123Z","level":"info","message":"order placed","logger":"shop","order_id":42,"total":19.5}
```

| Field | From |
|---|---|
| `timestamp` | the record's creation time, UTC ISO 8601 with milliseconds |
| `level` | the level name, lowercase (`warning` stays `warning`; the agent maps it to `status:warn`) |
| `message` | the formatted message |
| `logger` | the logger name |
| `exception`, `error.kind` | the formatted traceback and the exception class, when there is one |
| `trace_id`, `span_id` | the record's attributes of those names (`extra=`; the tracer sets them from M5) |
| `service`, `env`, `version` | the constructor's arguments, else `OZY_SERVICE`, `OZY_ENV`, `OZY_VERSION` |
| anything else in `extra=` | a field of the same name, searchable as `@name:value` |

- **A traceback is one event.** JSON escapes newlines, so the whole stack is one line and one log; no
  multiline pattern is needed. Search it with `status:error "ValueError"`.
- **It never raises** and never writes two lines. A value that cannot be serialized becomes its `repr`, and a
  record that cannot be formatted at all becomes a line saying so, because a formatter that throws takes the
  log call down with it.
- **An `extra=` key that collides with a field above** (`level`, `message`, …) is kept as `<name>_`, so a
  caller cannot overwrite what the pipeline relies on.
- Secrets in extras are redacted by the agent, not here: the formatter does not know what a secret is. The
  agent's defaults cover keys like `password` and `token` (docs/operations.md, "Logs").

Point the agent at the output with a container label (`ozy.logs.enabled=true`; `ozy.logs.source` defaults to
`json`) or a file source.

## Fork behaviour

Gunicorn, Celery prefork and `multiprocessing` all `fork()` worker processes. A forked child
inherits the parent's memory but **none of its threads**. It can also inherit a lock that
another thread was holding at the moment of the fork, and that lock stays locked forever in
the child. When the SDK is enabled, it registers
`os.register_at_fork(after_in_child=...)`, which gives the child:

- fresh locks, so an inherited held lock can't deadlock it;
- an empty buffer, because messages buffered before the fork belong to the parent, which
  still sends them, and the child sending them too would double-count;
- its own socket, opened on the next send;
- a new flusher thread, started by the child's first metric;
- zeroed `stats()` counters.

The tracer's writer gets the same treatment (fresh locks, an empty queue, no thread until the
child's first finished trace; spans already buffered by a trace that started before the fork finish
and send from the child).

You don't need to call `init()` again in the worker, although doing so is harmless. On
platforms without `fork()`, the hook and its test are skipped.

## Why UDP

A UDP send is one non-blocking syscall, with no connection, no handshake and no
back-pressure. If the agent is down, slow or restarting, the kernel drops the datagram and
your request handler never notices. For metrics that's the right trade: they're statistical,
and they must never slow down or break the code they measure. The cost is best-effort
delivery, which is why `stats()` exists and why each datagram stays under one MTU (losing
one IP fragment would lose the whole datagram).

Two more details:

- The socket is **connected** (`connect()` on UDP only fixes the peer address). The benefit
  is that an ICMP "port unreachable" reply surfaces as `ConnectionRefusedError` on a later
  send and gets counted, instead of vanishing.
- DNS is resolved **at most once per 60 s**, and a failed lookup is cached for 60 s too,
  because `getaddrinfo` blocks. The periodic re-resolve still follows an agent whose address
  changes, such as a container restart behind a compose service name.

## Troubleshooting

| Symptom | Check |
|---|---|
| Nothing arrives, and `statsd.enabled` is `False` | `OZY_AGENT_HOST` isn't visible to the process. Check `init()` arguments too: `agent_host=""` disables |
| `stats().sent` climbs but the agent sees nothing | UDP was accepted locally and lost afterwards. The agent isn't listening on that host and port, or (on a Mac with Colima) UDP isn't forwarded into containers. See [operations.md → Colima](../operations.md#colima-udp-from-the-mac-doesnt-reach-containers). Run the agent natively (`make dev`) or switch Colima to `portForwarder: grpc` |
| `stats().errors` climbs | Set `OZY_DEBUG=1` and configure logging (`logging.basicConfig()`). Failures are logged on the `ozy` logger |
| Metrics stop after a fork | This shouldn't happen (see [Fork behaviour](#fork-behaviour)). Check that the child doesn't exit with `os._exit()` before a flush. Call `statsd.flush()` first |
| Lines are rejected as parse errors by the agent | Check the agent's `ozy.agent.statsd.parse_errors`. Common causes are an empty metric name, or a value from another client that isn't a finite number. Empty tag entries (`tags=["a", ""]`) are harmless, because the agent skips them |
| `tracer.enabled` is `False` | No agent host, or `OZY_TRACE_ENABLED=0`. `tracer.stats()` stays at zero |
| `tracer.stats().chunks_dropped` or `send_errors` climbs | The agent is down, busy (`429`) or unreachable on `OZY_TRACE_PORT` (default 8126, TCP). Traces are best-effort and never retried |
| A trace is missing the worker half | The worker's functions are not wrapped with `ozy.integrations.arq.traced`, or the API was deployed before the workers (see the arq section) |
| SQL / Redis / httpx spans never appear | They only record inside an active trace: check that an `http.request` (or other) span is active where the call is made, and that a thread was not started without `tracer.wrap_executor` |
| Metrics missing at shutdown | `atexit` doesn't run on `os._exit()` or on a SIGKILL. Call `statsd.flush()` or `statsd.close()` yourself |

## Developing the SDK

```sh
cd sdk/python
uv sync
uv run pytest                               # includes the 90% coverage gate
uv run ruff check && uv run ruff format --check
uv run mypy                                 # strict, src + tests
uv run python benchmarks/trace_overhead.py  # tracer overhead per span
```

The tracing tests run each integration against the real library, so the dev group carries
`fastapi`, `httpx`, `httpx2` (Starlette's `TestClient` warns without it, and warnings are errors),
`sqlalchemy`, `redis` and `arq`; the SDK itself still has no runtime dependencies. The arq end-to-end
test needs a Redis: `OZY_TEST_REDIS_URL=redis://localhost:6379/15 uv run pytest tests/test_trace_arq_e2e.py`
(it flushes that database, so never point it at database 0).

The contract suite (`tests/test_contract.py`) loads the shared goldens through a path
relative to the test file, so it needs a full checkout of the repository.
