# ozymandias

An observability platform built from scratch as a learning project — agent, ingestion pipeline,
time-series and log storage engines, distributed tracing, a query language, dashboards and
alerting. Built to be app-agnostic, and proved against three real applications as the
milestones land — a Next.js app on the Node SDK, a FastAPI service on the Python SDK, and a
Rails API with no SDK at all (Prometheus scrape, stdout logs, stock OpenTelemetry), covering
the three adoption styles.

**Status: M2 part one (the TSDB).** Metrics flow end to end — an app sends extended StatsD over
UDP, the agent aggregates into 10s buckets and forwards them batched and gzipped, and the
Metrics Explorer graphs them — and `ozyd` now stores them in a storage engine written from
scratch: a write-ahead log, Gorilla-compressed chunks, an inverted index, immutable blocks and
leveled compaction. Sketches and percentiles are next, then logs, traces and monitors. The
design as built is in [DESIGN.md](DESIGN.md), and the plan in [PLAN.md](PLAN.md).

```bash
# no SDK required — the protocol is the interface
printf 'my.metric:1|c|#env:dev\n' | nc -u -w1 localhost 8125
open http://localhost:9400/metrics/explorer
```

## Stack

- **Go** — agent and server (`ozyd`); what Prometheus, Loki and most production agents
  are written in, so their source doubles as reference material.
- **Own storage engines** — TSDB (WAL, Gorilla compression, inverted index, blocks) and a
  Loki-style log store, written from scratch because that is the point; Pebble for traces;
  SQLite for metadata.
- **Own wire protocol and SDKs** — zero-dependency Python and Node clients (statsd over UDP,
  traces over HTTP) instead of OpenTelemetry, to learn what an SDK actually does.
- **React + TypeScript + Vite + uPlot** — web UI, embedded into the server binary.
- **docker-compose** — local deployment.

## Setup

Requirements: Go 1.27+, Node 24 (`web/.nvmrc`), Docker with Compose,
golangci-lint 2.x and lefthook.

```bash
lefthook install        # once: commit-msg, pre-commit and pre-push hooks
make up                 # build the image; ozyd on :9400 (UI + API), agent on :8126 and :8125/udp
make dev                # or run both natively, with the Vite dev server on :9401
```

[docs/operations.md](docs/operations.md) covers configuration, ports and
troubleshooting, including a Colima UDP limitation.

## Testing

```bash
make ci                 # the full gate: lint, race tests + coverage gates, docs checks, web, short fuzz
make smoke              # end to end against the compose stack (after `make up`)
make fuzz-long          # every fuzz target for 10 minutes
```

Two gates ([ADR-0013](docs/adr/0013-actions-on-pull-requests.md)). The git
hooks are the fast one — every commit runs the checks for what's staged, every
push runs `make ci` — and GitHub Actions re-runs `make ci` on every pull
request from a clean checkout. `make smoke`, the crash loop and `fuzz-long`
stay manual, once per milestone.

## Docs

- [DESIGN.md](DESIGN.md) — how it works, as built
- [PLAN.md](PLAN.md) — architecture, decisions, milestone map
- [AGENTS.md](AGENTS.md) — how to work in this repo
- [CONTRIBUTING.md](CONTRIBUTING.md) — commits, PRs, ADRs
- [docs/wire-protocol.md](docs/wire-protocol.md) — every payload on every hop
- [docs/adr/](docs/adr/) — decision log
- [docs/operations.md](docs/operations.md) — running and configuring it
- [docs/api.md](docs/api.md) — the HTTP API
- [docs/metrics-catalog.md](docs/metrics-catalog.md) — every metric ozymandias emits about itself
- [docs/sdk/python.md](docs/sdk/python.md), [docs/sdk/node.md](docs/sdk/node.md) — the SDKs
- [docs/benchmarks.md](docs/benchmarks.md) — measured numbers for the hot paths
- [docs/notes/](docs/notes/) — what each milestone taught
- [SECURITY.md](SECURITY.md) — what is in scope, and what is knowingly missing

## Name

Ramesses II, by way of Shelley. A monument in the desert with an inscription
daring you to look on its works felt like the right name for one person's
attempt at an observability platform. It went by something else while the repo
was private; [ADR-0012](docs/adr/0012-public-repo-and-the-name.md) explains the
change.

## License

[MIT](LICENSE)
