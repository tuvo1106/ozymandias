# Changelog

All notable changes to the `ozymandias` Node.js SDK are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this package adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
