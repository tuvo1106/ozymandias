# Changelog

All notable changes to the `ozy` Python SDK are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
package adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- No code change: `format_number` already emitted wire-protocol §A's canonical
  number form, and §A now states that rule in its terms. The Node SDK and the
  Go writer were the two that disagreed outside `[1e-4, 1e16)`, and both were
  changed to match this one.

### Added

- **Tracing (M5).** `from ozy import tracer`: `trace()` (a `with` / `async with` context manager),
  `wrap()` (sync and async decorator), `start_span()`, `current_span()`, `current_trace_context()`,
  `inject()` / `extract()` for the `x-ozy-*` propagation headers, `wrap_executor()` and `stats()`.
  The active span lives in a `ContextVar` (survives `await`, copied into tasks, not into threads).
  Spans buffer per local root and flush as one chunk (partial past 500 spans, late chunks for spans
  that outlive their root); head sampling is the shared function of the trace id, with the rate from
  `OZY_TRACE_SAMPLE_RATE` and the agent's `rate_by_service`; unsampled traces are still sent. The writer
  is a bounded (1000 chunks, drop-oldest), fork-safe daemon thread posting `POST :8126/v1/traces` with a
  2 s timeout and no retries, draining on exit within 1 s. New config: `OZY_TRACE_ENABLED`,
  `OZY_TRACE_PORT`, `OZY_TRACE_SAMPLE_RATE` and `init(trace_enabled=, trace_port=, trace_sample_rate=,
  integrations=)`. Inert without an agent host.
- `ozy.normalize_path()`, identical to Go's `wire.NormalizePath`, and `Context`, `Span`, `Tracer`,
  `TracerStats` exported from `ozy`.
- Integrations behind a public `Integration` protocol, `register_integration()` and `patch_all()`:
  `ozy.integrations.asgi.TraceMiddleware` (the metrics middleware plus an `http.request` span, one
  middleware for both) and an `asgi` patch for Starlette/FastAPI apps; `sqlalchemy` (statement text,
  never parameters), `redis` (command name, never keys), `httpx` (header injection), `arq`
  (`inject_job_kwargs`, `traced` / `traced_job`, and an `enqueue_job` patch: one trace across the
  queue), and `logging` (`TraceLogFilter` and a record factory stamping `trace_id` / `span_id`).
- `benchmarks/trace_overhead.py`: tracer time and allocations per span, disabled / unsampled / sampled.
- Dev dependencies for the integration tests: `fastapi`, `httpx`, `httpx2`, `sqlalchemy`, `redis`, `arq`.
  The SDK itself still has no runtime dependencies.
- `ozy.integrations.logging.JSONFormatter`: a `logging` formatter that writes one JSON object per
  line (timestamp, level, message, logger, exception, `error.kind`, `trace_id`, `span_id`, service/env/version
  and every `extra=` field), so the agent reads fields rather than parsing text and a traceback is one event.
  It never raises. See docs/sdk/python.md.
- `ozy.integrations.asgi.MetricsMiddleware`: a pure ASGI middleware that records
  `http.request.count` and `http.request.duration` tagged by route pattern, method and
  status. It never raises into a request and is inert until `ozy.init()` enables the client.
- `ozy.init()`, which layers its arguments over the `OZY_*` environment variables.
  With no agent host configured, the SDK is disabled and inert.
- The `ozy.statsd` client: `increment`, `decrement`, `gauge`, `histogram`,
  `distribution`, `timing`, `set`, and `timed` (a context manager and a decorator for sync
  and async functions), plus `flush`, `close` and `stats`.
- Byte-exact extended StatsD formatting per `docs/wire-protocol.md` §A, checked against the shared
  `sdk-cases.json` goldens.
- Newline-coalesced buffering up to 1432 bytes per datagram. A lazily started daemon thread
  flushes every 100 ms, and it restarts in the child after `fork()`. `close()` runs at exit.
- A non-blocking UDP socket, with DNS cached for 60 s. Every error is swallowed and counted,
  and memory stays bounded when the agent is unreachable.
