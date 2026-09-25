# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **The project is now `ozymandias`, and the repo is public** (ADR-0012). The
  working name it carried while the repo was private was a pun on a commercial
  product — not a name to publish under. Everything typed uses the short token
  `ozy`: binaries
  `ozyd` and `agent`, env prefixes `OZY_` and `OZY_AGENT_`, self-metrics `ozy.*`,
  wire headers `X-Ozy-*`, SDK packages `ozy`, config at `/etc/ozy`, data at
  `./data/ozyd`. **Breaking for every existing deployment:** re-create the config
  from `deploy/ozyd.yaml` and `deploy/agent.yaml`.
- **Block magic is now `OZCH`/`OZIX`.** Blocks written by earlier builds cannot
  be read; there is no migration, by choice.
- **The wire dialect is described as "extended StatsD"** rather than by the
  vendor name for the same grammar. The protocol is unchanged and the
  compatibility captures from `datadogpy` and `hot-shots` still pass.
- **GitHub Actions runs `make ci` on every pull request** (ADR-0013, supersedes
  ADR-0010) now that public-repo minutes are free — including `make sdk-check`,
  which the metered workflow used to skip, so both SDKs' 90% gates are enforced
  remotely too. The git hooks remain the fast gate.
- **The SDKs stay vendored** (ADR-0014, supersedes ADR-0007) — now because they
  are pre-1.0, not because the repo is private. Git install is documented for
  Python; a first publish would use PyPI `ozy` and npm `@tuvo1106/ozy`.

### Fixed

- **`FakeClock.Advance` no longer costs one iteration per ticker period.** A
  ticker left behind by a big `Advance` delivers its first missed deadline and
  drops the rest — which the clock already did, one period at a time, re-sorting
  its waiters on each. `TestDB_RetentionCanBeDisabled` advances 10,000 hours
  against a 6s maintenance ticker and spent 249s of the `internal/tsdb/db`
  package's 299s doing it. The package now runs in 42s, with no test removed and
  no assertion changed.
- `make test` now passes `-timeout 25m`. `internal/tsdb/db` costs ~5 minutes
  under `-race` locally and ran 600.06s on a CI runner, which Go's 10-minute
  default killed mid-test.
- **`GET /api/v1/query` no longer panics on an out-of-range `from`/`to`.** The
  bucket count is computed from the raw parameters, and
  `?from=-9223372036854775808&to=1790000000&interval=10` wrapped it negative,
  slipping past the 10,000-bucket cap and reaching a `make([]float64, n)` that
  panicked outright. `from` and `to` are now required to be unix seconds in
  `[0, 253402300799]`, which is what the bucket arithmetic actually assumes.
- **A query's range is capped at 366 days.** The bucket cap bounds the answer,
  not the work: `?from=0&to=now&interval=200000` is a legal 8,951 buckets and
  asked the store to read 55 years of samples.

### Added

- `SECURITY.md` and `CODE_OF_CONDUCT.md`.

- **Samples are append-only** (ADR-0011). A sample at or before a series' newest timestamp is
  rejected and reported in `AppendResult.Rejected` instead of overwriting it; an exact repeat
  of the newest sample stays a no-op, so at-least-once agent retries are still safe. The naive
  SQLite store previously did last-write-wins and now matches, because the M2 TSDB stores
  samples in append-only Gorilla chunks and physically cannot overwrite one.

### Added

- **The real TSDB (M2).** Gorilla-compressed chunks, a segmented write-ahead log, an
  inverted index, an in-memory head, immutable on-disk blocks and leveled compaction, tied
  together by `internal/tsdb/db` behind the existing `MetricStore` interface. It is now the
  default metric store; `storage.metric_store: naive` selects the M1 SQLite store, which is
  kept as the differential-test oracle and as an escape hatch.
- `storage.*` configuration: block range, retention, disk cap, cardinality limit and WAL
  sync policy, each with a `OZY_STORAGE_*` override (see `docs/operations.md`).
- `ozy.tsdb.*` self-metrics: rejected samples, head size, block count and disk usage.
- A 50-iteration crash loop (real `SIGKILL`s of a child process), a 72-hour fake-clock
  lifecycle test, a concurrency stress test, format goldens and fuzz targets for the chunk
  and WAL decoders. Between those and three rounds of code review they found nine ways an
  acknowledged sample could be lost, two ways disk could leak, one way a metadata query
  could kill the process and one way shutdown could panic; all are fixed, and none ever
  shipped. See `docs/notes/M2.md`.
- `wal.Repair`, run before the log is opened for writing: a torn tail is now removed rather
  than left for the next append to queue up behind.

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
- Both binaries report their own `ozy.*` metrics through the pipeline, tagged `host:`.
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
