# ozymandias

An observability platform built from scratch as a learning project — agent, ingestion pipeline,
time-series and log storage engines, distributed tracing, a query language, dashboards and
alerting. Built to be app-agnostic; first used on [app-node](../app-node) (Node SDK),
[app-python](../app-python) (Python SDK) and [app-ruby](../app-ruby) (Ruby — no SDK: Prometheus
scrape, stdout logs, OpenTelemetry).

**Status: planning.** No code yet. Start with [PLAN.md](PLAN.md); the per-milestone specs are
in [docs/plan/](docs/plan/).

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

<!-- Filled in by M0 (docs/plan/M0-skeleton.md). Planned shape: -->

```bash
lefthook install        # once, after cloning
make up                 # ozyd on :9400 (UI + API), agent on :8125/udp + :8126
```

## Testing

<!-- Filled in by M0. Planned shape: -->

```bash
make test lint          # go test -race + coverage gates, golangci-lint
make smoke              # end-to-end against the compose stack
```

## Docs

- [PLAN.md](PLAN.md) — architecture, decisions, milestone map
- [AGENTS.md](AGENTS.md) — how to work in this repo
- [CONTRIBUTING.md](CONTRIBUTING.md) — commits, PRs, ADRs
- [docs/wire-protocol.md](docs/wire-protocol.md) — every payload on every hop
- [docs/adr/](docs/adr/) — decision log

## License

[MIT](LICENSE)
