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
