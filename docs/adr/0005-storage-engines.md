# ADR-0005: Build the TSDB and log store; use Pebble and SQLite for the rest

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

Storage engines are one of the four areas the owner wants to understand. Building every store from scratch would stall the project, and using existing engines everywhere would skip the lesson.

## Decision

Build the metric TSDB from scratch (write-ahead log, Gorilla chunks, inverted index, blocks, compaction; M2) and a Loki-style log store (M4). Store traces and sketches on Pebble, an embedded LSM key-value store, where the learning is key design rather than engine internals. Keep metadata (dashboards, monitors, keys) in SQLite via the pure-Go `modernc.org/sqlite`. The naive SQLite metric store from M1 stays forever as the differential-test oracle.

## Alternatives considered

| Option | Why not |
|---|---|
| ClickHouse, Postgres or Elasticsearch behind everything | The engines would be the part not learned. |
| From scratch for traces too | A third engine has diminishing returns; key design on an LSM is the transferable lesson. |

## Consequences

Two engines of real depth, with crash-safety and property testing requirements (docs/plan/testing.md L2, L5). The dependency allowlist in AGENTS.md bans the Prometheus TSDB packages and `sketches-go`, so the build-it lesson can't be skipped by accident.
