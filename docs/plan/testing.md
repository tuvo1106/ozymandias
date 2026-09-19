# Testing strategy (applies to every milestone)

Strong test coverage is a project requirement, not a nice-to-have. Storage
engines and telemetry pipelines fail in ways that only show up under crashes,
concurrency and malformed input — so the test plan is layered, and each
milestone spec lists the specific tests it must ship.

## 1. Coverage gates (enforced in CI, fail the build)

| Scope | Gate | Tool |
|---|---|---|
| Go, whole module | ≥ 80% statements | `go test -race -coverprofile` + `scripts/check-coverage.sh` |
| Go, critical packages: `internal/tsdb/...`, `internal/sketch`, `internal/query/...`, `internal/agent/statsd`, `internal/agent/aggregator`, `internal/monitor/...`, `pkg/wire` | ≥ 90% | same script, per-package thresholds in `scripts/coverage-thresholds.txt` |
| `sdk/python` | ≥ 90% lines + branches | `pytest --cov --cov-branch --cov-fail-under=90` |
| `sdk/node` | ≥ 90% lines/branches/functions | `vitest --coverage` thresholds in `vitest.config.ts` |
| `web/` | ≥ 70% on `src/lib/**` (pure logic: query builders, time math, flame-graph layout); components covered by smoke/e2e instead | `vitest --coverage` |
| App integrations | each app's own gate still passes (app-python backend: 80% fail-under) | their existing CI |

Excluded from Go coverage: `cmd/*/main.go` wiring, generated code. Nothing else.
A PR that lowers a threshold needs owner approval and an ADR.

Coverage is a floor, not the goal: the required test *kinds* below matter more
than the percentage.

## 2. Test layers

### L1 — Unit (fast, hermetic, the bulk)
Table-driven tests for every parser, encoder, state machine and aggregator.
No network, no real clock (`Clock` fake), disk only under `t.TempDir()`.

### L2 — Property tests (`pgregory.net/rapid`; `hypothesis` for the Python SDK)
Required wherever there is an encode/decode pair or an algebraic law:

- Gorilla chunk: `decode(encode(samples)) == samples` for arbitrary in-order
  samples, including NaN payload bits, ±Inf rejected upstream, ±0, denormals,
  huge timestamp gaps, identical timestamps deltas.
- WAL: any sequence of records round-trips; any truncation point of the last
  segment yields a valid prefix.
- Index postings: intersection/union/negation equal a naive set implementation.
- DDSketch: every quantile within relative error α of the exact quantile;
  `merge(a,b)` equals sketch of the concatenated input; merge is commutative
  and associative.
- Statsd: `parse(format(msg)) == msg`.
- Query parser: `parse(print(ast)) == ast`.
- Trace/propagation: `extract(inject(ctx)) == ctx`.

### L3 — Differential tests (oracle = the naive implementation)
The naive SQLite `MetricStore` from M1 is kept forever as an oracle. A
randomized workload is appended to both stores; every `Select` and every
metricql query must return identical results. Same pattern for LogStore v1
(grep) vs v2 (bloom-filtered).

### L4 — Fuzz tests (`go test -fuzz`, corpus committed)
Every function that parses bytes from outside the process: statsd datagram
parser, every `pkg/wire` JSON decoder, WAL segment reader, chunk decoder,
index reader, log-pipeline parsers, metricql and logql parsers, Docker log
stream demuxer. Invariant: never panic, never allocate unboundedly, never
loop forever. CI runs each fuzzer for 30s; `make fuzz-long` runs 10 min each.

### L5 — Crash and fault-injection tests
- **Crash recovery:** a test helper spawns `ozyd` as a subprocess, ingests
  acknowledged data, `SIGKILL`s at a random moment, restarts, and asserts every
  acknowledged sample/log/span is present and no corrupt data is served.
  Run in a loop (≥ 50 iterations in CI, seed logged on failure).
- **Torn writes:** truncate / bit-flip WAL segments, chunk files, index files
  and assert: detected via checksum, recovered to a valid prefix or the block
  quarantined — never a panic, never silently wrong data.
- **Faulty filesystem:** storage takes an `FS` interface; a fault-injecting
  implementation returns ENOSPC / EIO / short writes at scripted points.
- **Faulty network:** forwarder and SDK writers are tested against an
  `httptest.Server` that scripts timeouts, resets, 429s with `Retry-After`,
  500s, and slow-loris responses.

### L6 — Concurrency
All Go tests run with `-race`. Head append, WAL, live-tail hub, aggregator and
concentrator each get a stress test: N writers + M readers for a fixed
duration, asserting invariants (no lost samples, monotonic timestamps per
series). Goroutine-leak check (`goleak`-style helper written in-repo: compare
`runtime.NumGoroutine` / stack dump before and after) on every component with
a `Close()`.

### L7 — Golden files
- `pkg/wire/testdata/` shared by Go + both SDKs (see wire-protocol.md).
- On-disk format goldens: a committed block directory, WAL segment and log
  chunk written by version N must be readable by every later version. A format
  change requires a version bump in the file header and a new golden.
- API response goldens for `/api/v1/*` (with `-update` flag to regenerate).

### L8 — Integration (in-process)
`ozyd` and `agent` constructed in-process with temp dirs, fake clock and
ephemeral ports: statsd packet in → query API out; log line appended to a temp
file → `/api/v1/logs` out; span POSTed → trace API out; monitor transitions →
fake notifier captures.

### L9 — End-to-end smoke (`scripts/smoke.sh`, real binaries, docker compose)
Grows each milestone; must stay under 2 minutes. Run by hand (`make up && make smoke`) and
quoted in the PR; there is no remote CI (§4). Sends real UDP/HTTP traffic,
polls the public API until data appears, exits non-zero with diagnostics on
timeout. Runs in CI on every push.

### L10 — SDK contract tests
Each SDK test suite starts a fake agent (UDP + HTTP listener in the test
process), exercises the public API, and asserts the exact bytes/JSON against
the wire goldens. Plus the **safety suite**, identical in both SDKs:

- agent unreachable / DNS failure / port closed → no exception, no unbounded
  memory, bounded queue drops oldest, drop counter increments;
- agent hangs → app call returns in < 1 ms (enqueue only), writer times out at 2s;
- `OZY_AGENT_HOST` unset → no socket, thread, timer or patch is created
  (assert via thread/handle enumeration);
- an integration target library missing → import succeeds, integration no-ops;
- exceptions inside user code within a span propagate unchanged, span is
  finished with `error=1`;
- fork / worker-process safety for Python (writer thread restarts after fork);
- Next.js double-load: two module instances share one state via `globalThis`.

### L11 — UI
Vitest unit tests for `web/src/lib`; React Testing Library for the query
editor and time picker; one Playwright happy-path per page (run against the
smoke stack) from M3 on.

### L12 — Benchmarks and load (tracked, not gated)
`go test -bench` on hot paths; `cmd/loadgen` scenarios. Results go in
`docs/notes/M<n>.md` and `docs/benchmarks.md` (one table, appended per
milestone) so regressions are visible.

## 3. Conventions

- Test names state behaviour: `TestAggregator_CounterScalesBySampleRate`.
- One assertion helper package `internal/testutil` (fake clock, temp store
  builders, fake agent/intake servers, fault FS, eventually-poller). No
  third-party assertion library.
- Randomized tests log their seed and accept `-seed` to reproduce.
- No `time.Sleep` in tests — use the fake clock or the `Eventually` poller.
- A bug fix lands with a test that fails without the fix.
- Tests are documented too: a non-obvious test gets a comment stating what
  failure mode it guards against.

## 4. The gate: git hooks (no remote CI)

GitHub Actions is off (ADR-0010). The repo is private and the Free plan's
minutes are used up by other repos. **The git hooks are the CI:**

| Hook | Runs | Typical cost |
|---|---|---|
| pre-commit | Only the checks the staged file types need: gofmt, golangci-lint, race tests with coverage gates (cached, so only affected packages re-run), docs drift, no app coupling, web typecheck, ESLint and related Vitest tests | 1–3 s warm |
| pre-push | `make ci`: everything, over the whole tree, plus web coverage thresholds and a 10 s-per-target fuzz pass | tens of seconds |
| by hand | `make smoke` (L9), `make fuzz-long` (L4), the crash loop (L5, from M2) | per milestone |

The PR description carries the evidence: `make ci` output, plus `make smoke`
when runtime behaviour changed. The one-job workflow in
`.github/workflows/ci.yml` stays for manual runs. Restore its `pull_request`
trigger if minutes ever become available, and move smoke, the crash loop and
fuzz back into it.

Every milestone spec has a **Test plan** section naming the concrete tests
required at each layer. A milestone is not done until those tests exist and pass.
