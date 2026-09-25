# ADR-0004: Two binaries: a per-host agent and a modular-monolith server

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

A production observability backend is dozens of services. Copying that shape locally would spend the learning budget on deployment instead of mechanisms, but a single process would hide the seams that make the real system scale.

## Decision

Ship two binaries. `agent` runs per host: it aggregates, collects, samples and forwards. `ozyd` is intake, stores, query, monitors and UI in one process, with hard internal interfaces (`MetricStore`, `LogStore`, `TraceStore`, `SketchStore`). M7 then puts a durable queue between intake and the stores, when there is a working system to feel the difference in.

## Alternatives considered

| Option | Why not |
|---|---|
| Microservices from the start | This means more deployment and plumbing and fewer mechanisms, and the queue's value is invisible before there is load and failure to show it. |
| No agent, apps send to the server | This loses edge aggregation, host and container visibility, and the isolation of apps from server outages, which are three core lessons. |

## Consequences

One process to run and debug locally. The interfaces must be respected by convention and review, since nothing enforces them at runtime.
