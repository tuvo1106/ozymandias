# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Metrics, end to end (M1).** An extended StatsD counter sent from an app is aggregated by the
  agent, forwarded to `ozyd`, stored, and queried back through the UI.
- `pkg/wire`: the shared name/tag rules and the `/v1/series` payload, with goldens both SDKs
  and the Go code load, so the three implementations cannot drift.
- Agent statsd server: a zero-allocation extended StatsD parser (45 ns/line), multiple UDP readers
  feeding workers through a bounded queue that drops and counts rather than growing.
- Agent aggregator: sharded contexts, 10s buckets, sample-rate scaling, counter zero-fill with
  expiry, gauge last-write-wins, histogram reservoir with `.avg`/`.min`/`.max`/`.median`/
  `.95percentile`/`.count`, and late samples folded into the oldest open bucket.
- Agent forwarder: payloads split at 5000 series / 2 MiB, gzipped, retried with full-jitter
  backoff honouring `Retry-After`, buffered in a 64 MiB drop-oldest queue, with a final
  delivery attempt on SIGTERM.
- `ozyd` intake `POST /v1/series` with per-series rejection, a metadata DB that pins a
  metric's first-seen type, and the `MetricStore` interface with a naive SQLite implementation.
- Query API `GET /api/v1/query` plus `/metrics`, `/tags` and `/tags/values`, with bucketing,
  grouping and cross-series aggregation; empty buckets serialize as `null`.
- Metrics Explorer UI at `/metrics/explorer`: autocomplete, filter chips, group-by, aggregator,
  time range, auto-refresh and a uPlot chart, with the query state in the URL.
- Zero-dependency SDKs: `sdk/python` (Python ≥ 3.12) and `sdk/node` (Node ≥ 22, ESM + CJS),
  identical in shape, each wrapped so they cannot throw into the host app.
- Both binaries report their own `ozymandias.*` metrics through the pipeline, tagged `host:`.
- `cmd/loadgen` (`statsd-flood`) and `examples/cron-script.sh` — a metric from a shell script
  with `nc`, no SDK.
- extended StatsD compatibility goldens captured from datadogpy and hot-shots, with
  `scripts/capture-statsd-compat.sh` to refresh them.
- Docs: DESIGN.md §9 metric write path (with diagram) and §10 aggregation model,
  `docs/benchmarks.md`, the agent config reference and a metric-not-arriving checklist in
  `docs/operations.md`, and M1 learning notes.

- Go module `github.com/tuvo1106/ozymandias`, Makefile entry points, golangci-lint v2 config,
  pre-commit hooks for Go formatting and lint, and CI with the coverage, docs-drift,
  no-app-coupling and fuzz gates from `docs/plan/testing.md` and `documentation.md`.
- `ozyd` and `agent` binaries: `/healthz`, `/debug/vars` self-metrics, graceful shutdown
  on SIGTERM, and a `healthcheck` subcommand for distroless images.
- Layered config (defaults < file < `conf.d` fragments < env) with strict unknown-key
  checking; commented reference files `deploy/ozyd.yaml` and `deploy/agent.yaml`
  (ADR-0009).
- Test support: fake clock, `Eventually`, goroutine-leak checker.
- Web UI shell (Vite, React, TypeScript, Tailwind) embedded in `ozyd`: sidebar with every
  planned section and its milestone, server status on the home page, SPA routing with
  immutable asset caching.
- Docker image (distroless, non-root, ~28 MB, UI embedded) and compose stack; `make up` waits
  until both services are healthy.
- `make smoke`: 14 end-to-end checks against the running stack, including clean exit on SIGTERM.
- `make dev`: both binaries natively plus the Vite dev server, stopped together.
- Docs: DESIGN.md (as built), docs/operations.md, ADRs 0002–0008 and 0010, M0 learning notes.

### Changed

- The git hooks are the CI (ADR-0010). pre-commit runs lint, race tests with coverage gates,
  docs and coupling checks, and the web typecheck, lint and related tests for whatever is
  staged; pre-push runs the full `make ci`. The GitHub Actions workflow is manual-only.

### Fixed

- Config: a YAML file holding only comments loads as empty instead of failing with `EOF`.
- Config: a null value (e.g. `tags:` with every item commented out) merges as absent, so a
  later fragment's list still appends.
- `testutil.FakeClock`: `Stop` and `Reset` discard an unread fire, matching Go 1.23+ timers.
