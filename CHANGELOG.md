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
- **The workflows' actions are current again, and stay that way.** Five had
  fallen far enough behind that GitHub was force-running them on a newer Node
  than they declared: checkout v4→v7, setup-go v5→v7, setup-node v4→v7,
  setup-uv v5→v10, golangci-lint-action v8→v9. A grouped monthly Dependabot
  config now watches them, so the next drift arrives as one pull request
  rather than as a warning nobody is reading. `ubuntu-latest` is kept on
  purpose through the Ubuntu 26 migration; the reasoning is in the workflow.
- **GitHub Actions runs `make ci` on every pull request** (ADR-0013, supersedes
  ADR-0010) now that public-repo minutes are free — including `make sdk-check`,
  which the metered workflow used to skip, so both SDKs' 90% gates are enforced
  remotely too. The git hooks remain the fast gate.
- **The SDKs stay vendored** (ADR-0014, supersedes ADR-0007) — now because they
  are pre-1.0, not because the repo is private. Git install is documented for
  Python; a first publish would use PyPI `ozy` and npm `@tuvo1106/ozy`.

### Added

- **Boundary tests for the chunk encoder's delta-of-delta buckets**, found by a
  one-off mutation-testing run over `internal/tsdb/chunkenc`. A delta-of-delta
  filed one bucket too wide still decodes to the right number — it only spends
  more bits — so no round-trip test could see it. The new tests assert the
  *cost in bits* of every bucket edge, which is what the bucket is, plus the
  duplicate-timestamp case on the delta-of-delta path, which the existing table
  covered for the first two samples but not the third.

### Fixed

- **Eight storage defects from a whole-tree review** (issues #4–#11), all in the
  M2 engine:
  - `block`: a chunk record's length prefix was validated with `used+length+4
    == recLen`, which overflows — a ten-byte uvarint can wrap the sum back onto
    `recLen`, and the slice taken next wrapped too, panicking on data that came
    from `index.dat`. An offset near the top of the range wrapped the same way.
    `chunks.dat` was also the only on-disk decoder in the TSDB with no fuzz
    target, which is why this survived three review rounds; it has one now.
  - `head`: a series record's tag count was bounded by the record length but a
    tag costs two bytes and `tsdb.Tag` is 32, so a record could make the
    decoder reserve 32× its own size — half a gigabyte at the 16 MiB WAL
    ceiling — before parsing a byte. The head's record decoders gained a fuzz
    target too.
  - `head`: an `Append` batch above ~600k samples was encoded as a single WAL
    record and rejected for exceeding `MaxRecordSize`, so a dense intake
    request could never succeed and every retry failed identically. Batches are
    now split across records in one `wal.Log` call, which keeps log order.
  - `db`: `CutBlock` could freeze the head *above* wall-clock time. The cut
    threshold bounds `cutAt` by the head's MaxT, and `wire.MaxFutureSkew`
    accepts samples ten minutes ahead, so with a short `block_range` one
    fast-clocked client made the store reject every real-time sample with
    `ErrOutOfBounds` until the clock caught up.
  - `db`: a failed `block.Open` after a successful `block.Write` left the block
    on disk untracked, and the next tick wrote a second copy of the same
    samples. The cut is now all-or-nothing.
  - `block`: `Delete` fsynced the tombstone but not its directory, so a crash
    mid-unlink could leave a block with missing files and no tombstone — which
    is the one state that stops ozyd starting.
  - `compact`: the run scan stopped at the first block of another level or
    resolution. Once rollups land they interleave with their sources, every run
    would end at length one, and compaction would stop permanently for both
    resolutions. It now skips rather than stops.
  - `config`: `storage.retention: 0` read as "off" (as `max_bytes: 0` does) but
    reached the store as unset and came back as the 15-day default, quietly
    deleting data. It is now rejected, with the negative form named in the
    error.
- **A query landing on a block cut could return a hole** (issue #3). `DB.Select`
  snapshotted the block list, read every block, and only then read the head. A
  cut inside that window publishes its block *after* the snapshot and truncates
  the head *before* the head read, so the block range it moved was in neither
  half of the answer — a successful query, missing a contiguous range, with
  nothing logged. The head is now read first, which is the order the cut's own
  publish-then-truncate sequence was written to be safe against; the samples are
  briefly in both places instead of neither, and the merge deduplicates them.
  Not a durability bug: nothing was ever missing from disk.
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
- **One StatsD line can no longer poison an agent's whole flush.** A sample
  rate is used as its reciprocal, so `x:1|c|@1e-320` passed the `(0,1]` check
  and scaled a single increment to `+Inf` — which never leaves the bucket, and
  which `wire.Point` then refuses to marshal. The parser rejects a rate that
  small, and the aggregator drops any sample whose scaled value is not finite.
- **The forwarder skips a series it cannot encode instead of dropping the
  batch.** One unencodable series used to fail the whole payload, discarding
  every other series in that flush — including the agent's own self-metrics —
  once per interval for as long as the bad input kept arriving.
- **`scripts/check-docs.sh` checks self-metrics again.** Its pattern still
  looked for `ozymandias.*` names, so after the rename it matched none of the
  33 registered metrics and passed whether or not they were documented — in
  the pre-commit hook and in `make ci` alike. It now matches `ozy.*` and fails
  if it ever matches nothing at all.
- **`make sdk-release` no longer aborts on the Python wheel.** The filename
  was hard-coded as `ozymandias-*.whl` while the package is `ozy`, so the
  script announced a file `uv build` had never produced and would have failed
  at the copy the first time app-python was a target. The name now comes from
  `pyproject.toml`, and a mismatch fails at the build step.
- **A re-run of an interrupted WAL truncation no longer loses records.** A
  `Truncate(n)` that published `checkpoint.n` and then died partway through
  deleting the segments it was built from left the next pass — which computes
  the same `n` — rebuilding the checkpoint from the segments that survived and
  renaming over the one that held the rest. Anything that lived only in the
  already-deleted segments was in no block and no log. The rebuild now reads
  the checkpoint at `n` as one of its sources.
- **A chunk that fails to decode fails the read instead of shortening it.**
  `head.Select` stopped at the bad chunk and returned the samples it had,
  which a caller cannot tell from a series that is genuinely that short — and
  its caller is the block cut, which writes a block from that snapshot and
  then truncates the head and the log to match. One decode error dropped that
  chunk and every later chunk of the series from all three. `Head.Select` now
  returns an error, as `block.Block` already did, and a cut that cannot read
  the head is abandoned with everything still in place.
- **The canonical number form on the wire is now one form, not three.**
  wire-protocol.md §A said "the shortest string that round-trips a float64",
  which every implementation satisfied while writing different bytes: Go's
  `strconv` `'g'` turns a byte count of `1048576` into `1.048576e+06`,
  JavaScript's `String` stays positional to `1e21`, and Python's `repr`
  switches at `1e16`. §A now specifies the window (positional in `[1e-4,
  1e16)`, exponent form outside it, exponent padded to two digits), the Go
  writer and the Node SDK implement it, and seven golden cases pin it. Every
  form parsed back to the same float64, so nothing was ever corrupted — but
  the SDKs' promise of identical bytes was not true, and the goldens could
  not catch it because no case fell in the divergent windows.
- **The Node SDK drops a non-numeric value instead of coercing it.** From the
  CJS build, `gauge("queue.depth", null)` recorded a real `0` — as did `[]`,
  while `"5"` recorded `5` — where the Python SDK refuses all three.
- **CI runs the fuzz targets.** The workflow claimed parity with `make ci`
  minus smoke, the crash loop and `fuzz-long`, but `make ci` also runs
  `make fuzz FUZZTIME=10s` and no step did — leaving the guard on the
  corrupt-input decoders enforced only by the local pre-push hook.
- **CI runs on every pull request**, with no `paths-ignore`. `ci` is a required
  status check on `main` now, and a required check that is never *reported* is
  not a check that passed: a skipped workflow leaves the pull request waiting
  on a status that will never arrive, so a docs-only change could never be
  merged. Running it is not a no-op either — `make ci` includes
  `make docs-check`.
- **Compaction deletes its source blocks only once the merged block is
  serving.** `compact.Run` unlinked them before the database had opened the
  merged block or swapped it in, so a failure in either step left blocks gone
  from disk and still in `db.Blocks()` — and the next tick planned the
  identical run and wrote another full copy of the merged block, every tick,
  all of it charged against `MaxBytes`. Deleting is now `compact.DeleteSources`,
  called after the swap, and a source that will not delete is logged and left
  for `dropSuperseded` rather than failing a compaction that has happened.
- **Damage in the last WAL segment is only a torn tail if it reaches the end of
  the file.** Being in the last segment was the whole test, so a bad checksum
  at offset 0 of a 32 MiB segment discarded every record after it with no
  error, no log line and no counter — and `Repair`'s `TruncateTail` then made
  it permanent. A crash can only tear the record being appended, so damage
  with whole records written after it is corruption and is now reported as
  `ErrCorrupt`. Where the damaged record's extent is known (a checksum
  mismatch means every promised byte was there), the bound is exact.
- **Two blocks covering the same time range no longer hide each other.** The
  cross-source merge in `Select` deduplicated only against the last sample it
  had appended, which is correct only if the sources are prefix-ordered in
  time. They are not after a crash between writing a merged block and deleting
  its sources, and will not be once rollup blocks land: every sample the second
  source held for an instant the first had nothing for was dropped. Samples are
  now merged, sorted and deduplicated by timestamp when any source arrives out
  of order, with the oldest source still winning an instant they both claim.
- **Group-commit has its own goroutine.** It shared one select with the
  maintenance pass, so no WAL fsync happened for the whole of a block cut, a
  compaction rewriting three blocks and a retention sweep. With
  `wal_sync_on_append: false` the window an acknowledged sample spends in the
  page cache was therefore not `wal_sync_interval` — as `Options.SyncInterval`
  and `deploy/ozyd.yaml` both say — but the length of the longest pass.

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
- `ozy.tsdb.wal_syncs`: write-ahead log flushes since startup. Monotonic, so its
  *rate* is the signal — a flat stretch means acknowledged samples are staying in
  the page cache longer than `wal_sync_interval`.
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
