# AGENTS.md

Working notes for anyone (human or AI) contributing to this repo — how to work in the code,
and why things are the way they are. [`PLAN.md`](PLAN.md) and `docs/plan/` (and, once code
exists, `DESIGN.md`) are the source of truth for *what* we're building; this file is *how*.

This repo follows the owner's `the project template`. The **Conventions** section below is the
template's, verbatim in substance; everything after it is ozymandias-specific. If the two ever
disagree, Conventions wins.

## Conventions

- Commit messages: [Conventional Commits](https://www.conventionalcommits.org/)
  (`type(scope): subject`, ≤ 72 chars, scope matching `[a-z0-9-]+`); **no `Co-Authored-By`
  trailer.** Enforced by a `commit-msg` hook — run `lefthook install` once after cloning. See
  [CONTRIBUTING.md](CONTRIBUTING.md) for the full commit/PR/ADR process.
- **Never commit directly to `main`.** One PR per feature. Branch prefixes:
  `feat/<slug>`, `fix/<slug>`, `chore/<slug>`, `docs/<slug>`.
- **Build as if this repo were public.** No secrets, credentials, tokens, or
  real user data committed — ever, not "temporarily," not in a branch you plan
  to squash. Config comes from environment variables (`.env`, gitignored; keep
  `.env.example` current). A `no-secrets` pre-commit hook backs this up but isn't a
  substitute for not doing it in the first place. This applies to test fixtures too:
  log lines captured from the apps for testdata (M4) must be scrubbed of real emails,
  tokens and user ids before they are committed.
- `CHANGELOG.md` ([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)) is updated under
  `[Unreleased]` in the same PR as anything user-visible.
- A decision that's hard to reverse, non-obvious, or reached by rejecting a plausible
  alternative gets an ADR in `docs/adr/` using `adr-template.md` (see ADR-0001). ADRs are
  immutable once accepted — supersede, don't edit.
- PRs fill out `.github/PULL_REQUEST_TEMPLATE.md`.

## 1. This is a learning project

The owner is building this to understand observability internals. That changes
what "done" means:

- **At the start of each milestone, ask the owner which pieces (if any) they
  want to write by hand.** Likely candidates: Gorilla encoder (M2), DDSketch
  (M2), query parser (M3), tracer context propagation (M5). For those, scaffold
  the package, interfaces and failing tests, and stop.
- **Code teaches the why.** Every Go package gets a `doc.go` framing the mental
  model. Exported types/functions explain reasoning, invariants and rejected
  alternatives — not what the next line does. Same standard for the SDKs
  (docstrings / JSDoc).
- **Every milestone ends with `docs/notes/M<n>.md`**: what was built, how the
  mechanism works, trade-offs taken, how production systems (Prometheus, Loki,
  Tempo) differ,
  what was surprising, measured numbers. This is a deliverable, not overhead.

## 2. Workflow

1. Work milestones in order. Do not start M(n+1) until every acceptance
   criterion of M(n) passes and is demonstrated (command + output in the notes).
2. **Two gates, local first** (ADR-0013). So:
   - **Batch.** One PR per milestone, or per large coherent chunk of one, not
     one per feature slice. Slices are still separate, well-described
     *commits* on the branch, so history stays reviewable.
   - **The git hooks are the fast gate.** pre-commit runs the checks for what
     you staged, and pre-push runs the full `make ci`. Never `--no-verify` a
     push. GitHub Actions re-runs `make ci` on every PR — the repo is public,
     so the minutes are free — but it is the backstop, not the thing you wait
     on. Put the local evidence (`make ci`, and `make smoke` when runtime
     behaviour changed) in the PR description either way.
   - Scopes are package-ish: `feat(agent): …`, `test(tsdb): …`,
     `feat(sdk-python): …`, `feat(web): …`.
   - The repo is `github.com/tuvo1106/ozymandias` (public). Open PRs with `gh`.
     **Merging:** you may merge a PR yourself (`gh pr merge --merge`) once a
     `/code-review` of its changes has run and every finding is fixed or
     explicitly answered. Otherwise the owner merges.
3. If a spec is wrong, ambiguous, or blocked by reality: pick the simplest
   option consistent with PLAN.md §4 principles, write an ADR for it (the
   plan's version is the rejected alternative; note the impact on later
   milestones), and amend the spec in the same PR. Do not silently diverge;
   do not stall on it.
4. `docs/wire-protocol.md` is normative. Changing a payload means updating that
   doc, `pkg/wire`, both SDKs and the tests in the same commit.
5. Report honestly: failing tests, skipped criteria and unverified claims get
   said plainly.

## 3. Definition of done (per task)

Three standards are binding on all work and are enforced in CI from M0 —
the two below plus [`docs/plan/extensibility.md`](docs/plan/extensibility.md)
(no app-specific code in the core; protocols and extension points are public,
versioned surfaces; app-node and app-python are the first users, not the
only ones):
[`docs/plan/testing.md`](docs/plan/testing.md) (coverage gates: 80% Go overall,
90% critical packages and both SDKs; required property / differential / fuzz /
crash / golden tests) and [`docs/plan/documentation.md`](docs/plan/documentation.md)
(doc set, `doc.go` + exported-symbol comments, format specs, learning notes).
Each milestone spec's **Test plan** and **Docs deliverables** sections are part
of its acceptance criteria — not optional follow-ups. Write tests alongside
the code (test-first for parsers, encoders and state machines).

- `go build ./... && go vet ./... && go test -race ./...` pass.
- `golangci-lint run` passes.
- SDK changes: `uv run pytest` + `ruff check` in `sdk/python`; `npm test` +
  `npm run typecheck` in `sdk/node`.
- UI changes: `npm run typecheck && npm run lint && npm test` in `web/`.
- `scripts/smoke.sh` passes (from M1 on; it grows each milestone).
- Docs updated in the same commit.

## 4. Go conventions

- Standard library first. `log/slog` for logging, `net/http` (Go 1.22+ pattern
  routing) for servers, `flag` + env + YAML for config, `testing` for tests.
- No global state; constructors take dependencies. Every time-dependent
  component takes a `Clock` interface (`Now() time.Time`, `NewTicker`), every
  disk component takes a root dir.
- Errors wrapped with `%w` and context. No panics outside `main` and tests.
- Every goroutine has an owner, a `context.Context`, and a shutdown path.
  `ozyd` and `agent` shut down gracefully on SIGTERM (flush, fsync, close).
- Hot paths (statsd parse, chunk append, WAL write) get benchmarks
  (`go test -bench`), with numbers recorded in the milestone notes.
- Storage code gets property tests (`pgregory.net/rapid`) and crash tests.

### Dependency allowlist (ask before adding anything else)

| Module | Used for |
|---|---|
| `gopkg.in/yaml.v3` | config files |
| `modernc.org/sqlite` | metadata DB, naive stores (pure Go, no cgo) |
| `github.com/klauspost/compress` | zstd for log chunks / sketches |
| `github.com/cockroachdb/pebble` | TraceStore, SketchStore |
| `github.com/shirou/gopsutil/v4` | host metrics |
| `github.com/oklog/ulid/v2` | block ids |
| `pgregory.net/rapid` | property tests |
| `github.com/jackc/pgx/v5` | agent `postgres` check only (M3; recommended, needs its ADR) |
| `google.golang.org/protobuf` | **test-only** oracle for the hand-written OTLP decoder (M8; recommended) |

Deliberately **not** allowed, because writing them is the point: Prometheus
TSDB packages, `DataDog/sketches-go`, OpenTelemetry SDKs, a Docker client
library (talk to `/var/run/docker.sock` with `net/http` directly), parser
generators, statsd libraries.

## 5. SDK conventions

- **Zero runtime dependencies** in both SDKs. Integrations import their target
  library lazily and no-op if it is absent.
- Python ≥ 3.12, `uv`, `ruff`, `pytest`; package name `ozymandias`.
- Node ≥ 22, TypeScript, ESM + CJS builds via `tsc`; package name `ozymandias`;
  tests with `vitest`. All process-wide state lives on
  `globalThis[Symbol.for("ozy")]` (Next.js dev can load a module twice).
- Public API is identical in shape across both SDKs (see M1 / M5 specs).
- Every public entry point is wrapped so it cannot throw into the host app.

## 6. Touching the instrumented apps

`../app-node`, `../app-python` and `../app-ruby` are real projects with their own rules.
They are referred to by role here because this repo is public (ADR-0012); their
real names, paths and per-milestone integration points live in
`docs/private/integrations.md`, which is gitignored. If that file is not on
your machine, you are not the person who should be touching them.

- **Read that repo's `AGENTS.md` / `CLAUDE.md` first** and follow it over this
  file when they conflict.
- Work on a `feat/ozymandias-<slug>` branch in that repo (both follow the same
  template conventions). Never commit to its default branch; never push
  without the owner asking.
- app-python: commit messages carry **no `Co-Authored-By` trailer**; docs
  (DESIGN.md §2.1 for a new tool, AGENTS.md, CHANGELOG.md, README.md, diagrams)
  update in the same commit; backend has an 80% coverage gate and `ruff`.
- app-ruby: **public repo**, like this one — commit nothing there that
  references private artifacts or keys; `DESIGN.md` is the spec and changes
  to it go in their own PR *before* the implementation PR; fixed commit-scope
  list; mandatory PR template with a Design reference; 90% coverage gate;
  `customer_phone` must never reach ozymandias. It gets **no ozymandias SDK** —
  config, labels and stock OpenTelemetry only (`docs/private/integrations.md` §3).
- app-node: layering rule — integration code lives in `src/lib/`, nothing
  above `db/` imports better-sqlite3; "a feature gated on configuration is
  absent, not broken"; docs update with the change.
- The integration must be inert with `OZY_AGENT_HOST` unset. Prove it by
  running each app's full test suite with ozymandias stopped.
- Keep each integration change minimal and confined to the seams listed in
  `docs/private/integrations.md`.

## 7. Commands

```
make help           list every target
make build          go build ./cmd/... into ./bin (embeds the UI if built)
make web            build the UI into internal/api/ui/dist
make ci             the full local gate; lefthook runs it on git push
make test / lint / docs-check / web-check / fuzz / fuzz-long
make up / down / down-v   the compose stack (waits until healthy)
make smoke          end-to-end checks against the running stack
make dev            ozyd + agent natively, plus Vite on :9401
make sdk-release    (M1) build SDK artifacts into the apps' vendor/ dirs
```

Environment gotchas, found the hard way:

- **Colima doesn't forward UDP from the Mac** into containers (ssh port
  forwarder). See docs/operations.md.
- macOS ships bash 3.2 and no `timeout` command, so scripts must not rely on
  `wait -n`, `mapfile` or `timeout`.
- `web/node_modules` contains a Go package (npm's `flatted`). `go.mod`'s
  `ignore` directive and `.golangci.yml` exclude it.
