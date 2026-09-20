# Python SDK

`ozymandias` sends metrics from any Python 3.12+ application to an ozymandias agent. It has no
runtime dependencies, it never raises into your code, and it does nothing at all until you
point it at an agent.

M1 ships the statsd client. Tracing and the framework integrations arrive in M5
(see [PLAN.md](../../PLAN.md)).

The SDK is a convenience layer. The public interface is the wire protocol
([`docs/wire-protocol.md`](../wire-protocol.md) §A), so any extended StatsD client can talk to the
agent. The SDK exists to get the fiddly parts right once: formatting, buffering, sampling,
fork safety and failure isolation.

## Install

The package is not on PyPI yet. The repository is private and the name has not been claimed
([extensibility.md](../plan/extensibility.md) §5). There are three ways to install it:

| From | Command |
|---|---|
| PyPI (future) | `pip install ozymandias` / `uv add ozymandias` |
| git | `pip install "git+https://github.com/tuvo1106/ozymandias#subdirectory=sdk/python"` |
| a built wheel | `cd sdk/python && uv build`, then `pip install dist/ozy-0.1.0-py3-none-any.whl` |

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
| `debug` | `OZY_DEBUG` | off | `1`/`true`/`yes`/`on`: send failures go to the `ozymandias` logger at WARNING and payloads at DEBUG |
| `max_payload` | none | `1432` | Maximum datagram size in bytes (one Ethernet MTU minus headers). The agent reads at most 8192 |
| `flush_interval` | none | `0.1` | Seconds between background flushes |

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
`configure(ozymandias.Config(...))` enables it.

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
| `stats().errors` climbs | Set `OZY_DEBUG=1` and configure logging (`logging.basicConfig()`). Failures are logged on the `ozymandias` logger |
| Metrics stop after a fork | This shouldn't happen (see [Fork behaviour](#fork-behaviour)). Check that the child doesn't exit with `os._exit()` before a flush. Call `statsd.flush()` first |
| Lines are rejected as parse errors by the agent | Check the agent's `ozy.agent.statsd.parse_errors`. Common causes are an empty metric name, or a value from another client that isn't a finite number. Empty tag entries (`tags=["a", ""]`) are harmless, because the agent skips them |
| Metrics missing at shutdown | `atexit` doesn't run on `os._exit()` or on a SIGKILL. Call `statsd.flush()` or `statsd.close()` yourself |

## Developing the SDK

```sh
cd sdk/python
uv sync
uv run pytest                               # includes the 90% coverage gate
uv run ruff check && uv run ruff format --check
uv run mypy                                 # strict, src + tests
```

The contract suite (`tests/test_contract.py`) loads the shared goldens through a path
relative to the test file, so it needs a full checkout of the repository.
