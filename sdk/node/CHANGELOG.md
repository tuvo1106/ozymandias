# Changelog

All notable changes to the `ozy` Node.js SDK are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this package adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Tracing (M5).** `tracer.trace / wrap / startSpan / scope / inject / extract / stats / flush`
  on an `AsyncLocalStorage` context; head sampling with the shared BigInt algorithm; a bounded
  (1000 chunks, drop-oldest) writer to `POST /v1/traces` that reads `rate_by_service`, never
  keeps the process alive and flushes on `beforeExit` within 1 s. New config: `OZY_TRACE_ENABLED`,
  `OZY_TRACE_PORT`, `OZY_TRACE_SAMPLE_RATE` (`traceEnabled`, `tracePort`, `traceSampleRate`,
  `integrations` in `init()`).
- **Integrations** behind a public `Integration` interface and `registerIntegration()`:
  `withTelemetry` / `traceRoute` for Next.js App Router handlers, `instrumentSqlite(db)` for
  better-sqlite3, `instrumentFetch({ propagateTo })` (header injection only for allow-listed
  hosts) and `traceFormat()` for winston. There is deliberately no require-hook auto-patcher.
- `normalizePath()` and `sampleKeep()` exports, checked against the shared Go vectors.

### Fixed

- **Numbers outside `[1e-4, 1e16)` now match the Python SDK byte for byte.**
  `formatNumber` was `String(value)`, whose thresholds are JavaScript's own:
  `1e16` came out as `10000000000000000` against Python's `1e+16`, `0.00001`
  as `0.00001` against `1e-05`, and `1e-7` as `1e-7` against `1e-07`. All of
  them parse back to the same float64, so nothing was corrupted — but the two
  SDKs promise each other identical bytes, the shared goldens compare bytes,
  and no golden case fell in those windows to catch it. The canonical form is
  now specified in wire-protocol.md §A and pinned by seven new golden cases.
- **A value that is not a number is dropped and counted, not coerced.**
  `metric()` called `Number(value)` first, so from the CJS build an untyped
  caller's `gauge("queue.depth", null)` recorded a real `0`, as did `[]`, and
  `"5"` recorded `5`. The Python SDK refuses all three. Non-numbers now take
  the same path as NaN and ±Infinity: no datagram, `stats().dropped` goes up.

### Added

- extended StatsD metrics client: `init()` and `statsd.increment`, `decrement`, `gauge`,
  `histogram`, `distribution`, `timing`, `set`, `timed`, `flush`, `close`, `stats`.
- Configuration from `OZY_*` environment variables, overridden by `init()` options.
  Without `OZY_AGENT_HOST` the SDK is a no-op and allocates nothing.
- Client-side sampling, buffering (1432-byte datagrams, 100 ms unref'd flush timer), lazy
  connected UDP socket with a 60 s DNS cache, exit-time flush.
- Process-wide state on `globalThis[Symbol.for("ozy")]`, so duplicate module instances
  (Next.js, ESM + CJS) share one client.
- ESM and CommonJS builds with TypeScript declarations; zero runtime dependencies.
