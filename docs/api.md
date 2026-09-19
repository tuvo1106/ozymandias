# HTTP API reference

Every HTTP route ozyd and the agent serve. `scripts/check-docs.sh` fails
CI if a route registered in code is missing here.

The telemetry intake endpoints (`/v1/*`) are specified byte-for-byte in
[wire-protocol.md](wire-protocol.md). This page covers everything else. The
query API (`/api/v1/*`) arrives in M1.

## Common to both binaries

### `GET /healthz`

Liveness: answers `200` whenever the process is serving. It is not a
readiness check. M7 adds `/readyz` for "stores open, WAL replayed".

```console
$ curl -s localhost:9400/healthz
{"component":"ozyd","status":"ok","uptime_seconds":42,"version":"v0.0.0-3-gabc123"}
```

| Field | Meaning |
|---|---|
| `status` | Always `"ok"`; a failing process doesn't answer at all |
| `component` | `ozyd` or `agent` |
| `version` | Build version from `git describe` (`dev` for an unstamped build) |
| `uptime_seconds` | Seconds since the process started, rounded |
| `hostname` | *(agent only)* The value of the `host` tag this agent applies |
| `intake_url` | *(agent only)* Where the agent forwards to |

Both images use this through their own `healthcheck` subcommand, because they
contain no curl (see `internal/cli`).

### `GET /debug/vars`

The process's self-metrics (the `ozymandias.*` instruments). They're catalogued
in [metrics-catalog.md](metrics-catalog.md).

```console
$ curl -s localhost:8126/debug/vars
{"metrics":[{"name":"ozy.build.info","type":"gauge","tags":["component:agent","version:dev"],"value":1}, …]}
```

Each entry has `name`, `type` (`counter` | `gauge`), `tags` (sorted
`key:value` strings) and `value`. `value` is `null` for a non-finite gauge.
Entries are sorted by name, then tags.

## ozyd only

### `GET /` (web UI)

Serves the embedded single-page UI (`internal/api`):

- Existing files are served as-is. Hashed files under `/assets/` are cached
  for a year (`immutable`).
- Any other path returns `index.html` with `Cache-Control: no-cache`, so
  client-side routes like `/dashboards/abc` survive a reload and a new build
  is picked up on the next load.
- A missing file under `/assets/` is a real `404`. It means a stale page is
  asking for an old hash, and serving HTML in place of JavaScript would fail
  confusingly.
- A binary built without the UI serves a page explaining `make web`.
