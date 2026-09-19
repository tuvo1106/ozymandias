# ozymandias

An observability platform built from scratch as a learning project — agent, ingestion pipeline,
time-series and log storage engines, distributed tracing, a query language, dashboards and
alerting. Built to be app-agnostic; first used on [app-node](../app-node) (Node SDK),
[app-python](../app-python) (Python SDK) and [app-ruby](../app-ruby) (Ruby — no SDK: Prometheus
scrape, stdout logs, OpenTelemetry).

**Status: M0 (skeleton).** Both binaries run with config, health and a UI shell. Telemetry arrives
in M1. The design as built is in [DESIGN.md](DESIGN.md), and the plan in [PLAN.md](PLAN.md).

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

There's no remote CI: the git hooks are the gate
([ADR-0010](docs/adr/0010-local-first-ci.md)). Every commit runs the fast
checks for what's staged, and every push runs `make ci`.

## Docs

- [DESIGN.md](DESIGN.md) — how it works, as built
- [PLAN.md](PLAN.md) — architecture, decisions, milestone map
- [AGENTS.md](AGENTS.md) — how to work in this repo
- [CONTRIBUTING.md](CONTRIBUTING.md) — commits, PRs, ADRs
- [docs/wire-protocol.md](docs/wire-protocol.md) — every payload on every hop
- [docs/adr/](docs/adr/) — decision log
- [docs/operations.md](docs/operations.md) — running and configuring it

## License

[MIT](LICENSE)
