# ADR-0057: Shed logs and traces before metrics; bound queries with a slot limit

- **Status:** Accepted
- **Date:** 2026-11-13

## Context

With the queue in place ([ADR-0056](0056-queued-intake-answers-on-durability.md)) a slow store makes a topic
grow. Until it reaches `pipeline.queue.max_bytes` intake answers `429` for every
signal at once, so a log flood that fills its topic is the only thing that costs
anyone data, but only because each topic has its own cap. Two things are missing:
a way to say which signal to give up first when a topic is *nearly* full, and a
bound on how much of the machine queries may take from ingest (M7 §2).

## Decision

**Shed by fraction of the cap, expensive signals first.** A topic at or above 80%
of its cap refuses `logs` and `traces` with `429` and `Retry-After: 5`; `series` and
`sketches` are refused only at 95%. Metrics are the cheapest per byte and the signal
the monitors read, so they keep the last fifth of the room. The thresholds are
constants (`intake.ShedExpensiveAt`, `ShedCheapAt`), not config: they encode a
priority, and a tunable priority is a way to configure the pipeline into dropping
metrics first. Rejected alternative: one global threshold across topics. Topics are
separate directories with separate caps, so one signal's backlog is not another's
pressure; a global figure would shed metrics because logs are slow.

Shedding is *ahead of* the hard cap: `ErrFull` still answers `429` at 100% and the
queue still never evicts. Each shed request is counted in
`ozy.intake.shed{topic}`, registered up front.

**Bound the read API with slots, a short queue and a fast 503.** `GET /api/*` takes one
of `query.max_concurrent` (8) slots; up to `query.max_queue` (32) more wait up to
`query.queue_wait` (2 s); anything beyond is `503` with `Retry-After: 1`. The live
tail is exempt: it is a GET that stays open, and counting it would let eight open
tabs lock out every query. Writes under `/api/` (dashboard edits) are not limited.

## Consequences

- Under sustained pressure a sender sees `429` for logs and traces about a fifth of
  a topic earlier than for metrics, and the agent's retry/backoff does the rest. The
  agent's disk buffer (PR 4) is what stops that from losing data.
- A dashboard with more than 40 simultaneous widget queries sees some `503`s on a
  page load; its widgets refetch.
- Isolation of the live tail, monitors and queries from ingest is by bounds, not
  by separate goroutine pools: the tail hub drops for a slow subscriber, the
  monitor scheduler runs a fixed worker pool, queries are slot-limited. Ingest
  has no pool: it is one goroutine per request, bounded by the queue.
