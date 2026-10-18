# ADR-0047: Monitors evaluate a window that ends behind now

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

A monitor reduces its window to one number and compares it. The newest data is
the least complete: an agent aggregates for ten seconds before it flushes, the
forwarder batches and may retry, and ozyd acknowledges only once the WAL is
synced. A window that ends at the moment of evaluation always contains a bucket
that is still filling. A count monitor ("errors > 5 in 5m") under-reads it,
and a "requests < 1" monitor fires on every evaluation for traffic that is
merely late.

## Decision

The window is `[now − delay − N, now − delay]`, with `delay =
evaluation_delay_s` (default 30 s), and its end is aligned down to the
evaluation step (10 s, the agent's flush). Evaluations are stamped with their
scheduled tick, not with when a worker reached them. A backtest replays the same
ticks.

## Alternatives considered

| Option | Why not |
|---|---|
| Evaluate at now | Every evaluation reads a partial bucket; low-side and count monitors flap. |
| Wait for a "watermark" from the agents | Needs per-agent progress tracking in ozyd for a benefit a fixed delay mostly gives. Prometheus and Datadog both use a fixed delay too. |
| Drop the newest bucket instead of delaying | The same thing with a fixed, undocumentable delay of one step; a forwarder retry after a 5xx can be later than that. |

## Consequences

Every alert is at least `delay` late, and "why didn't my monitor fire yet" has
one more term in its answer (docs/operations.md#why-didnt-my-monitor-fire). A
monitor can set the delay to 0 for data it knows is complete. Aligning the
window makes live and backtest values identical, which is what lets the editor's
markers be trusted.
