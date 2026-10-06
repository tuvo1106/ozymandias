# Node.js SDK (`ozy`)

The Node SDK is a small convenience layer over ozymandias's public wire protocol. In M1 it is
a extended-StatsD-compatible metrics client; M5 adds a tracer and explicit-wrapping
integrations for Next.js route handlers, better-sqlite3, `fetch` and winston
([section 10](#10-tracing)).
Anything that can send a UDP datagram can emit metrics to the agent without this SDK (see
[wire-protocol.md §A](../wire-protocol.md#a-statsd-datagram-sdk--agent-udp-8125)). The SDK
adds buffering, sampling, global tags and a guarantee that it cannot hurt the host app.

- Source: [`sdk/node/`](../../sdk/node/). Node ≥ 22, ESM and CommonJS, TypeScript types,
  **zero runtime dependencies**.
- Python equivalent: `docs/sdk/python.md`. The public API has the same shape in both SDKs.

## 1. Install

| Method | Command | Status |
|---|---|---|
| npm registry | `npm install @tuvo1106/ozy` | Not published yet: the SDK is pre-1.0 ([ADR-0014](../adr/0014-sdk-distribution-after-going-public.md)). The bare name `ozy` is taken on npm, so a first publish uses the scoped name. |
| Git | — | npm can't install a package from a subdirectory of a git repo, so build a tarball instead. |
| Built tarball | `cd sdk/node && npm ci && npm pack`, then `npm install ./path/to/ozy-0.1.0.tgz` in the app | **The supported method today.** `npm pack` runs the build through `prepack`. |

`make sdk-release` (M1) builds the tarball and copies it into each app's `vendor/`
directory, which the app's `package.json` points at (`"ozy": "file:vendor/ozy-0.1.0.tgz"`).

## 2. Quick start

```ts
import { init, statsd } from "ozy";          // or: const { init, statsd } = require("ozy")

init({ service: "checkout", env: "dev", version: "1.4.0" });

statsd.increment("http.request.count", 1, { tags: ["route:/api/items", "method:get", "status:200"] });
statsd.distribution("http.request.duration", 12.4, { tags: ["route:/api/items"] });
const thumbnail = await statsd.timed("image.resize.duration", () => resize(buf), { tags: ["size:sm"] });
```

Call `init()` once at startup. In Next.js, that means `register()` in `instrumentation.ts`.
Calling it again replaces the client, which is safe but unnecessary.

## 3. Configuration

`init()` options win over environment variables, which win over defaults. An empty string
counts as unset in both places.

| Env var | `init()` option | Default | Meaning |
|---|---|---|---|
| `OZY_AGENT_HOST` | `agentHost` | unset | Agent hostname or IP. **Unset → the SDK is a complete no-op.** |
| `OZY_STATSD_PORT` | `statsdPort` | `8125` | Agent extended StatsD UDP port. Invalid values fall back to the default. |
| `OZY_SERVICE` | `service` | unset | Sent as the `service:` tag |
| `OZY_ENV` | `env` | unset | Sent as the `env:` tag |
| `OZY_VERSION` | `version` | unset | Sent as the `version:` tag |
| `OZY_TAGS` | `tags` | none | Tags on every metric. The env var is a comma list (`team:core,region:eu`); the option *replaces* it. |
| `OZY_DEBUG` | `debug` | off | Log SDK events (connects, errors, drops) to stderr. `1`, `true`, `yes`, `on`. |
| `OZY_TRACE_ENABLED` | `traceEnabled` | on | Tracing switch (needs an agent host). Only `0`, `false`, `no`, `off` turn it off. |
| `OZY_TRACE_PORT` | `tracePort` | `8126` | Agent HTTP trace intake. Invalid values fall back to the default. |
| `OZY_TRACE_SAMPLE_RATE` | `traceSampleRate` | `1` | Default head-sampling rate, clamped into [0, 1]. The agent's per-service rates override it. |
| — | `integrations` | none | Names of integrations to patch at `init()` (see [10.5](#105-integrations)). |
| — | `maxPayloadBytes` | `1432` | Max bytes per datagram (one Ethernet MTU minus headers) |
| — | `flushIntervalMs` | `100` | Max time a metric waits in the buffer |
| — | `hooks` | — | Test seams: `random`, `now`, `createSocket`, `lookup`, `timers`, `log`. Apps leave it unset. |

## 4. API reference

`opts` is always `{ tags?: string[], sampleRate?: number }`.

| Call | Sends | Agent aggregation (per 10 s bucket) |
|---|---|---|
| `statsd.increment(name, value = 1, opts?)` | `name:value\|c` | Σ value / rate |
| `statsd.decrement(name, value = 1, opts?)` | `name:-value\|c` | same counter |
| `statsd.gauge(name, value, opts?)` | `name:value\|g` | last value |
| `statsd.histogram(name, value, opts?)` | `name:value\|h` | avg, min, max, median, p95, count |
| `statsd.distribution(name, value, opts?)` | `name:value\|d` | a sketch from M2 (histogram in M1) |
| `statsd.timing(name, ms, opts?)` | `name:ms\|ms` | as histogram |
| `statsd.set(name, member, opts?)` | `name:member\|s` | count of distinct members |
| `statsd.timed(name, fn, opts?)` | `timing(name, elapsed ms)` | — |
| `statsd.flush()` | the buffer, now | — |
| `await statsd.close()` | flush, then close the socket | — |
| `statsd.stats()` | `{sent, packets, dropped, errors}` | — |

- `increment` and `decrement` also accept `(name, opts)`, which is the same as a value of 1.
- `timed(name, fn)` calls `fn()` and returns what it returns. If `fn` returns a promise,
  `timed` returns a promise that settles the same way, and the time is taken when it
  settles. If `fn` throws or rejects, the time is still recorded and the **same error
  object** is rethrown. `fn` runs even when the SDK is disabled.
- `close()` resolves once buffered datagrams have been handed to the kernel. It waits at most
  2 s and never rejects. After `close()`, metric calls are ignored until the next `init()`.
- `stats()` counts **messages** (`sent`, `dropped`), **datagrams** (`packets`) and swallowed
  internal **errors**. A message dropped by sampling is not counted: that is sampling
  working, not a loss. Before `init()`, or while the SDK is disabled, every counter is 0.

## 5. What goes on the wire

The normative rules are in [wire-protocol.md §A, "What a client sends"](../wire-protocol.md).
The exact bytes for each call are the shared goldens in
[`pkg/wire/testdata/statsd/sdk-cases.json`](../../pkg/wire/testdata/statsd/sdk-cases.json).
The SDK's contract test (`sdk/node/test/contract.test.ts`) replays every case against a real
UDP listener.

- **Section order:** `name:value|type`, then `|@rate` (only when rate < 1), then `|#tags`.
- **Tags:** the call's tags first, then `init()`/`OZY_TAGS` tags, then `service:`,
  `env:` and `version:` for whichever are set. The agent sorts and de-duplicates tags, so
  order doesn't affect the series. It just makes the output byte-testable.
- **Numbers:** `String(n)`. JavaScript's number-to-string conversion already prints
  integral values without a decimal point (`1`) and everything else as the shortest string
  that round-trips a float64 (`12.4`, `0.30000000000000004`). Very large or very small
  magnitudes use exponent form (`1e+21`, `1e-7`). The agent parses values with Go's
  `strconv.ParseFloat`, which accepts that form. `-0` prints as `0`. **NaN and ±Infinity are
  dropped** client-side and counted in `dropped`.
- **Sampling:** with `sampleRate < 1`, a message is sent only when `random() < sampleRate`,
  and `|@rate` is written so the agent scales it back up by `1/rate`. A rate ≥ 1 (the
  default) always sends and writes no rate. A rate that is 0, negative or NaN never sends.
- **Sanitation:** `|`, `,` and newline become `_` in names, tags and set members. `:` also
  becomes `_` in names, because it ends the name. Nothing else changes: lowercasing,
  character sets and length limits are the agent's job, so the rules live in one place
  instead of three.
- **Set members:** numbers are sent in their string form (`42`).

## 6. Buffering and delivery

A metric call samples, formats one line, appends it to an in-memory buffer and returns. The
buffer goes out as **one datagram with lines joined by `\n`** when:

1. the next line would push it past `maxPayloadBytes`. The buffer is sent first and the line
   starts a new one. A single line longer than the limit is sent alone rather than truncated.
2. `flushIntervalMs` has passed since the first line entered an empty buffer. This uses a
   one-shot, **unref'd** timer, so an idle process holds no timer, and the timer never keeps
   a finished script alive.
3. `flush()` or `close()` is called, or the process is exiting:
   - `beforeExit` (the event loop drained) flushes, and the send completes normally.
   - `exit` (including `process.exit()`) flushes on a best-effort basis. Once the socket is
     connected, the datagram reaches the kernel synchronously, so it is delivered. Metrics
     recorded before the first connection finished are lost.
   - Signals trigger neither. The SDK doesn't install signal handlers, because that would
     change your app's shutdown. If you handle `SIGTERM` yourself, `await statsd.close()`
     in the handler.

The socket is created **lazily** on the first flush and **connected** to the agent's
address, so sends skip a per-datagram DNS lookup and ICMP errors are reported. It is
unref'd, so it never keeps the process alive. The hostname is resolved **at most once per
60 s** and preferring IPv4. `localhost` often resolves to `::1` first, and an agent
listening on IPv4 would never see those datagrams. If the address changes (for example, an
agent container restarts with a new IP), the SDK reconnects to the new address. After a
failed lookup, messages are dropped for 5 s before the next attempt. While a connection is
being set up, up to 64 datagrams wait, and the oldest are dropped first.

## 7. Safety guarantees

These are tested in `sdk/node/test/safety.test.ts` (the statsd part of
[testing.md L10](../plan/testing.md)).

| Guarantee | How |
|---|---|
| Disabled means inert | Without an agent host, `init()` creates nothing. The test checks that `dgram.createSocket`, `setTimeout` and `setInterval` are never called, that no `exit`/`beforeExit` listener is added, and that `process.getActiveResourcesInfo()` is unchanged. |
| Never throws | Every public entry point catches its own exceptions (including those from hostile arguments) and counts them in `errors`. `init()` can't fail app startup. |
| Your errors stay yours | `timed()` rethrows the original error object and still records the duration. |
| Agent down / DNS failure / port closed | Errors are counted and messages dropped. Nothing is thrown and memory stays bounded. A dgram socket with no `'error'` listener would crash the process, so the SDK always attaches one. |
| Never blocks | Metric calls only touch memory. All I/O happens later and asynchronously. |
| One client per process | State lives on `globalThis[Symbol.for("ozy")]`, so two copies of the module share one client and one socket. This covers Next.js bundles and a mix of ESM `import` and CJS `require`. |

## 8. Why UDP

A metric call must never slow down or break the request that makes it. UDP is
fire-and-forget: there's no connection to set up, no acknowledgement to wait for, and no
back-pressure when the agent is slow or down. The cost is **at-most-once delivery**: a
datagram can be lost, and nothing tells you. That's acceptable for metrics. They are
aggregated into 10 s buckets, so a lost increment barely changes a rate, while a request
handler blocked on a metrics backend is an outage. Traces (M5) go over HTTP instead, because
losing a span breaks the trace it belongs to.

Datagrams are kept to 1432 bytes (one Ethernet MTU minus IP/UDP headers). A larger datagram
is fragmented by IP, and losing any one fragment loses the whole datagram.

## 9. Troubleshooting

- **Nothing arrives, no errors.** Is `OZY_AGENT_HOST` set in the process that calls
  `init()`? Set `OZY_DEBUG=1`: the SDK logs `disabled: OZY_AGENT_HOST is not set`,
  or `connected to <ip>:<port>` once it sends.
- **macOS with Colima: metrics from the host never reach a containerized agent.** Colima's
  default port forwarder only forwards TCP, so UDP from the Mac is dropped silently. Run the
  agent natively (`make dev`) or switch Colima to the gRPC forwarder. See
  [operations.md, "Colima: UDP from the Mac doesn't reach containers"](../operations.md#colima-udp-from-the-mac-doesnt-reach-containers).
- **`stats().errors` keeps growing.** With debug on, each error is logged. `ECONNREFUSED`
  means nothing is listening on that port (the kernel reports ICMP port-unreachable on the
  connected socket). `ENOTFOUND` means the hostname doesn't resolve from inside this process
  or container.
- **`stats().dropped` grows but `errors` doesn't.** You are sending NaN or ±Infinity values.
- **Short-lived scripts lose their last metrics.** If the script ends with `process.exit()`
  before the first connection finished, the last metrics are lost. `await statsd.close()`
  before exiting.
- **Only some metrics are sent.** Check for `sampleRate` below 1. `stats().sent` counts only
  what was sampled in.

## 10. Tracing

Tracing (M5) uses the same `init()`: with an agent host configured and `OZY_TRACE_ENABLED`
not off, `init()` installs a tracer. Without an agent host the tracer is inert: spans are
no-ops, **your callbacks still run**, no timer, socket or listener exists.

The wire format is [wire-protocol.md §B](../wire-protocol.md#b-traces-sdk--agent-post-8126v1traces);
this section is how the Node SDK implements it.

### 10.1 Quick start

```ts
import { init, tracer } from "ozy";

init({ service: "web", env: "dev" });          // OZY_AGENT_HOST must be set

const rows = await tracer.trace("db.load", { resource: "comics", type: "db" }, async (span) => {
  span.setTag("comic.id", id).setMetric("rows", 3);
  return loadRows();                           // a throw here marks the span and propagates unchanged
});
```

### 10.2 API

| Call | Meaning |
|---|---|
| `tracer.trace(name, opts?, fn)` | Runs `fn(span)` in a new span: a child of the active span, or a new trace root. Finishes when `fn` returns or its promise settles. Returns `fn`'s result (a promise stays a promise). |
| `tracer.wrap(name, opts, fn)` | `fn` wrapped so each call is `trace(name, opts, …)`; `this` and arguments pass through. |
| `tracer.scope().active()` | The active span, or `null`. `scope().activate(span, fn)` runs `fn` with a manually started span active. |
| `tracer.startSpan(name, opts?)` | Manual API. Does **not** activate the span; you must `finish()` it. A span that is never finished is simply absent from its trace. |
| `tracer.inject(ctx?, carrier)` | Writes `x-ozy-trace-id`, `x-ozy-parent-id`, `x-ozy-sampling-priority` into a plain object or a `Headers`. Default context: the active span. |
| `tracer.extract(source)` | Reads them from a plain object (case-insensitive), `Headers` or `Request`. Returns a context, or `null` for absent **or malformed** headers (a corrupt header never continues a corrupt trace). Pass it as `childOf`. |
| `tracer.stats()` | `spans`, `unsampled`, `chunksQueued`, `chunksSent`, `chunksDropped`, `errors`, `queued`. |
| `tracer.flush()` | Sends what is queued now; never rejects. |

`opts`: `resource` (default: the name), `type` (`web`, `db`, `cache`, `queue`, `http`, `worker`,
`custom`; anything else becomes `custom`), `service` (default: `init`'s; a span in another
service is `_top_level`), `childOf` (a span, a context, or `null` to force a new root), `tags`.

Span methods: `setTag`, `setMetric`, `setResource`, `setError`, `finish` (idempotent),
`context()`. Everything is chainable where it returns anything and none of it throws.

### 10.3 How it works

- **Context** is an `AsyncLocalStorage` kept on `globalThis[Symbol.for("ozy")]`, so two copies
  of the package share one context.
- **Ids** come from `crypto.randomBytes` (128-bit trace, 64-bit span, lowercase hex, never
  zero), drawn from a 4 KiB pool to avoid one call per span.
- **Time.** `start` is wall-clock microseconds, `duration` comes from `process.hrtime`. The wall
  start is derived from one (wall, monotonic) anchor so it has real microsecond resolution, and
  the anchor is re-taken when the two clocks disagree by more than a second (the monotonic clock
  pauses while a laptop sleeps).
- **Errors.** A thrown or rejected error sets `error=1`, `error.type`, `error.message`,
  `error.stack`, and is re-thrown as the same object.
- **Buffering.** Finished spans collect in a per-trace buffer. When the local root finishes,
  the buffer goes to the writer as one chunk. A trace past 500 finished spans flushes a partial
  chunk; a span that finishes after its root goes out as a late chunk. Every chunk's first span
  carries `_sampling_priority`.
- **`_top_level=1`** is set on a span whose parent is absent or in another service.
- **Head sampling** is decided once at the trace root with exactly the §B algorithm, in `BigInt`
  (`low64(trace_id) * 1111111111111111111 mod 2^64 < rate * 2^64`), so Node, Python and Go agree
  on every trace id (`sdk/node/test/trace-wire.test.ts` checks all of
  `pkg/wire/testdata/traces/sampling.json`). The rate is `OZY_TRACE_SAMPLE_RATE`, replaced per
  service by the agent's `rate_by_service` answer (key `service:<name>,env:<env>`). A trace that
  continues an extracted context inherits the upstream priority. **An unsampled trace is still
  sent** with priority 0: the agent needs every span for its request statistics.
- **Normalization.** The SDK applies the agent's own limits before sending (resource and meta
  values 5000 bytes on a rune boundary, 100 meta and 50 metrics entries, unknown type to
  `custom`, service and name 100 bytes, non-finite metrics dropped), so it never sends a span
  the agent would refuse whole.

### 10.4 The writer

A bounded queue of 1000 chunks (**drop-oldest**, counted in `chunksDropped`); a flush every
1 s (a timer armed by the first queued chunk, `unref`'d) or at 100 chunks; one request in
flight; `POST http://<agent host>:<OZY_TRACE_PORT>/v1/traces` with a 2 s timeout and **no
retries** (a retry holds spans in memory exactly when things are already slow); requests are
split to stay under 8 MiB. It uses `node:http`, not `fetch`, so the fetch integration never
traces the tracer. The socket is `unref`'d; the only time the writer holds the event loop is
the exit flush on `beforeExit`, for at most 1 s. A script that traces and exits delivers its
last trace; a hung agent cannot delay exit by more than that second. (Tested in a real child
process in `writer.test.ts`.) After `process.exit()` nothing can be sent: `exit` allows no I/O.
Await `tracer.flush()` first if you need the last spans.

### 10.5 Integrations

Every integration implements the public `Integration` interface
(`name`, `isAvailable()`, `patch()`, `unpatch()`), is registered with
`registerIntegration()`, and uses **only the public tracer API**, so an outside integration
has the same power as the built-in ones. `init({ integrations: ["fetch"] })` patches by name;
`patchIntegrations()` / `unpatchIntegrations()` do it by hand.

| Integration | Use | Span |
|---|---|---|
| `next` | `withTelemetry(handler, { route? })` wraps an App Router route handler; `traceRoute(req, routeHint, fn)` is the primitive it is built on, for an app that already has a single door for its handlers. | `http.request`, type `web`, resource `"GET /api/comics/:id"` from the path normalizer (or `routeHint`), `http.method`, `http.route`, `http.url` (no query), `http.status_code`. 5xx marks it failed; a throw is recorded as 500 and re-thrown. Continues a trace from `x-ozy-*` headers. |
| `sqlite` | `instrumentSqlite(db)` wraps `prepare` on the better-sqlite3 `Database` prototype so each statement's `run/get/all/iterate` is timed. | `sqlite.query`, type `db`, resource = statement source, `db.rowcount` for `run`. **Only inside an active trace** (no orphan roots from boot-time work). Parameters are never read, so they cannot be recorded; a test plants a sentinel value and scans every payload. A throwing statement marks the span and re-throws unchanged. `iterate()` ends its span when the iterator finishes or is abandoned. |
| `fetch` | `instrumentFetch({ propagateTo: ["api.internal", "*.corp.test"] })` wraps `globalThis.fetch` once. | `http.client`, type `http`, resource `"GET host"`, `http.url` without query or credentials. Only inside an active trace. Propagation headers go **only** to allow-listed hosts (host, host:port or `*.suffix`; default none, so a third party never sees your trace ids). It wraps whatever `fetch` is at that moment, so Next's own patch chain stays intact; a second call (or copy) only updates the allow-list; `Response` identity, rejections and abort behaviour are untouched. The span ends when headers arrive, not when the body is read. |
| `winston` | `traceFormat()` in `winston.format.combine(...)` (or `traceFormat(winston.format)`). | none: adds `trace_id` / `span_id` of the active span to each log entry. winston is not a dependency; the format is built structurally. |

For `next`, `fetch` and `winston` the SDK wraps explicitly; `unpatch()` on `next` and `winston`
only switches them off. `sqlite.unpatch()` restores every prototype it wrapped.

Next.js sketch:

```ts
// instrumentation.ts
import { init, instrumentFetch } from "ozy";
export function register() {
  init({ service: "web", env: "dev" });
  instrumentFetch({ propagateTo: ["image-service.internal"] });
}
// app/api/comics/[id]/route.ts
import { withTelemetry } from "ozy";
export const GET = withTelemetry(async (req, ctx) => Response.json(await load(ctx)), { route: "/api/comics/[id]" });
```

List the package in `serverExternalPackages` so exactly one instance loads (state on
`globalThis` is the second line of defence).

**Writing your own integration:**

```ts
import { registerIntegration, tracer, type Integration } from "ozy";
const redis: Integration = {
  name: "redis",
  isAvailable: () => { try { require.resolve("ioredis"); return true; } catch { return false; } },
  patch() { /* wrap a method; call tracer.trace("redis.command", { resource: cmd, type: "cache" }, …) */ },
  unpatch() { /* restore it */ },
};
registerIntegration(redis);
```

### 10.6 Why explicit wrapping, not a require hook

dd-trace patches libraries by hooking `require` (via `require-in-the-middle`, and
`import-in-the-middle` for ESM). That does not work for a Next.js app: Next bundles server
code, so the modules an app imports are already inlined into bundles and the hook never sees
them; reaching into Next's internals instead would break with every minor release. This SDK
therefore has **no auto-patcher**. An app wraps the few seams it owns (its route handlers, its
database handle, `fetch`) explicitly, which also makes it obvious in the code what is traced.
Hitting this wall is part of the lesson; the milestone notes tell the story.

### 10.7 Overhead

Measured with `scripts/bench-trace.mjs` (`npm run build`, then
`node --expose-gc --max-semi-space-size=256 scripts/bench-trace.mjs`); no budget is set, the
numbers are recorded so a regression is visible. See `docs/notes/M5.md` for the run recorded
with the milestone.

Run on an Apple-silicon laptop, Node 25.8 (the package requires 22+), medians of 200 slices
of 500 calls; time is the synchronous cost the request pays, allocation is heap growth per span
including the writer's serialized chunk (approximate: single batch, no GC):

| Shape | disabled | enabled, unsampled | enabled, sampled |
|---|---|---|---|
| single-span trace (root) | 0.01 us, ~90 B | 0.88 us, ~4.5 KB | 0.87 us, ~4.6 KB |
| 10-span trace, per span | 0.01 us, ~110 B | 0.55 us, ~2.5 KB | 0.55 us, ~2.5 KB |

Unsampled costs the same as sampled by design: an unsampled trace is still built and sent
(priority 0) because the agent needs every span for its statistics. The saving from a low
sample rate happens in the agent and store, not in the SDK.

### 10.8 Tracing safety guarantees

| Guarantee | How |
|---|---|
| Disabled means inert | No agent host (or `OZY_TRACE_ENABLED=false`): spans are no-ops, callbacks run, no timer, socket or listener exists. |
| Never throws | Span bookkeeping is wrapped; the callback runs whatever happens. |
| Your errors stay yours | A thrown or rejected error is re-thrown as the same object. |
| Never keeps the process alive | The flush timer and request socket are `unref`'d; the exit flush is capped at 1 s. |
| Bounded memory | 1000 queued chunks, 500 finished spans per trace before a partial flush. |
| No secrets | SQL parameters are never read; URLs are recorded without query or credentials; headers are only injected for allow-listed hosts. |
| Two copies of the package | Runtime, async context and the fetch wrapper's record live on `globalThis`; a second copy shares them. |
