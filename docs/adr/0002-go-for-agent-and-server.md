# ADR-0002: Go for the agent and the server

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

The project exists to learn how observability systems work underneath: agent, ingestion, storage engines, tracing, query. The language is chosen once and carries every milestone. The work is dominated by UDP and HTTP servers, byte-level storage formats, concurrency, and shipping a small agent binary.

## Decision

Write the agent and `ozyd` in Go (latest stable, currently 1.27). The UI is TypeScript (ADR-0006), and the SDKs are written in their apps' languages (ADR-0003).

## Alternatives considered

| Option | Why not |
|---|---|
| Rust | It's the best fit for storage-engine work, but iteration is slower, and the project is large enough that this roughly doubles the timeline. |
| TypeScript (one language everywhere) | It's weak for the byte-level storage and agent work, which are the parts most worth learning. |
| Python | It's a poor fit for a high-throughput intake and a binary storage engine. |

## Consequences

Prometheus, Loki, Tempo and most production agents are all Go, so their source reads as reference material for each milestone. Both binaries are static and cross-compile trivially (the image is under 30 MB). Garbage-collection pauses are an accepted cost; the storage milestones measure them rather than design around them up front.
