# Node.js SDK (`ozymandias`)

The Node SDK is a small convenience layer over ozymandias's public wire protocol. In M1 it is
a extended-StatsD-compatible metrics client. Tracing and framework integrations come in M5.
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
