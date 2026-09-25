# Documentation standard (applies to every milestone)

The owner is building ozymandias to *understand* observability systems.
Documentation is therefore a primary deliverable: if the code works but a
reader can't learn the mechanism from the repo, the milestone is not done.

## 1. Document set

| Doc | Purpose | Updated when |
|---|---|---|
| `README.md` | **Getting started, nothing else**: one-liner, Stack, Setup, Testing, doc links, License, plus a single status line pointing at PLAN.md. Screenshots and tours live in `docs/ui.md` / `docs/onboarding.md` | run/build/test steps change |
| `CONTRIBUTING.md` | Template file: commit/PR/ADR process and "where things live" (extended for this repo's doc set) | process changes |
| `PLAN.md`, `docs/plan/*` | The plan. Specs are edited to match reality in the same PR as a deviation, which is justified by an ADR | a spec turns out wrong |
| `DESIGN.md` | **Living architecture doc** — how the system works *as built*: data flow, each component's mechanism, on-disk formats, config reference, design decisions with trade-off and rejected alternative. Created in M0, grows every milestone | any architectural change, same commit |
| `docs/wire-protocol.md` | Normative payload spec | any payload change, same commit |
| `docs/formats/*.md` | Byte-level on-disk format specs: `wal.md`, `chunk.md`, `block-index.md`, `log-chunk.md`, `tracestore-keys.md`, `queue-segment.md`. Precise enough to write an independent reader | format change (+ version bump) |
| `docs/api.md` | HTTP API reference for `/api/v1/*`: params, response schema, errors, curl example per endpoint | endpoint change |
| `docs/query-language.md` | metricql + logql + monitor-query grammar (EBNF), semantics, worked examples | grammar/semantics change |
| `docs/sdk/python.md`, `docs/sdk/node.md` | SDK guides: install, config env vars, API reference, each integration, safety guarantees, troubleshooting | SDK change |
| `docs/operations.md` | Running it: config reference for `agent.yaml` / `ozyd.yaml`, data dir layout, retention, backup, `ozy.*` self-metrics catalog, troubleshooting playbook | config/ops change |
| `docs/metrics-catalog.md` | Every metric the integrations and the system emit: name, type, tags, unit, where emitted | metric added/changed |
| `docs/adr/NNNN-title.md` | Architecture Decision Records per ADR-0001 (the template's), written from `adr-template.md` (Status, Date, Context, Decision, Alternatives-considered table, Consequences). Immutable once accepted — supersede, never edit. Seed ADR-0002… from the PLAN.md §1 decisions. **A departure from the plan is an ADR** ("reached by rejecting a plausible alternative" — the plan's) | a significant decision is made |
| `docs/notes/M<n>.md` | **Learning notes** per milestone (template §3) | end of each milestone |
| `docs/diagrams/*.mmd` | Mermaid sources: system overview, metric write path, TSDB read path, log path, trace path incl. arq hop, monitor state machine. Embedded copies in DESIGN.md kept identical | a depicted flow changes |
| `docs/benchmarks.md` | One running table of benchmark/loadgen results per milestone | each milestone |
| `CHANGELOG.md` | Keep a Changelog + SemVer (template). Entries go under `[Unreleased]` in the PR that ships them, flagged **[protocol]/[sdk]/[api]/[config]** when a public surface changes; each completed milestone cuts a release + git tag (M1 → `v0.1.0` … M7 → `v0.7.0`) | anything user-visible |
| `docs/onboarding.md`, `docs/extending.md` | Instrument a new app; one worked example per extension point (see `extensibility.md`) | an extension point changes |

## 2. Code documentation

**Go**
- Every package has a `doc.go` with a package comment that frames the mental
  model: what problem this package solves, the core idea, the invariants, how
  it relates to neighbours, and what real systems do differently. Reference
  depth: a reader new to the topic should understand *why this design* before
  reading any function. Storage packages include ASCII diagrams of layouts.
- Every exported identifier has a doc comment (enforced by `revive`'s
  `exported` rule in golangci-lint). Comments explain reasoning, invariants,
  concurrency contract (who may call, from which goroutine, what is locked)
  and error semantics — not a restatement of the signature.
- Bit-level and lock-ordering code gets inline `// why:` comments.
- Runnable `Example…` functions (they are tests too) for the main entry points:
  `chunkenc`, `wal`, `sketch`, `metricql.Parse`, `tsdb.Open/Append/Select`.

**Python SDK** — module docstring framing purpose; Google-style docstrings with
`Args/Returns/Raises` on all public API; `ruff` `D` rules enabled for
`ozymandias/` (not tests).

**Node SDK / web** — JSDoc (`/** … */`) on every exported function, class,
hook and component; `eslint-plugin-jsdoc` `require-jsdoc` on exports.

**Config files** — `deploy/agent.yaml` and `deploy/ozyd.yaml` are fully
commented reference configs: every key present with default, unit and effect.

## 3. Learning-notes template (`docs/notes/M<n>.md`)

```
# M<n> — <title>
## What was built            (one paragraph + diagram)
## How it works              (walk a single datum through the code, with file:line links)
## Key concepts learned      (the transferable ideas, in the owner's terms)
## Trade-offs taken          (what we chose, what it costs, when it would be wrong)
## How the real systems differ   (Prometheus / Loki / Tempo / Jaeger)
## Measured numbers          (benchmarks, compression ratios, latencies — with commands)
## Surprises and bugs        (root causes, not just symptoms)
## Acceptance evidence       (each criterion → command + trimmed output or screenshot)
## Exercises                 (3–5 things to try/break to deepen understanding)
```

## 4. Docs in the instrumented apps

Each app's own rules win. app-python: new tool → DESIGN.md §2.1 entry (why /
trade-off / alternative), AGENTS.md conventions + gotchas, README run steps,
CHANGELOG, and `docs/*.mmd` diagrams if the submit flow picture changes (the
trace-context hop through arq does change `submission-flow.mmd`). app-node:
DESIGN.md / ARCHITECTURE.md (a new seam: `Telemetry`), ENGINEERING_NOTES.md for
decisions and gotchas (e.g. Next.js double module load), README env vars,
CHANGELOG.

## 5. Enforcement (CI `docs` job)

- golangci-lint `revive:exported` + a check that every `internal/*` and
  `pkg/*` package has a `doc.go`.
- `ruff` D-rules; eslint jsdoc rule.
- Markdown link checker over `*.md`; Mermaid files parse (`mmdc --dry-run` or
  equivalent).
- `scripts/check-docs.sh`: every metric name emitted in code appears in
  `docs/metrics-catalog.md`; every `/api/v1` route registered in
  `internal/api` appears in `docs/api.md`; every config struct field appears
  in the reference YAML. (Simple grep/AST scripts — cheap, and they stop drift.)
- Milestone gate: `docs/notes/M<n>.md` exists with all template sections
  filled, including acceptance evidence.
