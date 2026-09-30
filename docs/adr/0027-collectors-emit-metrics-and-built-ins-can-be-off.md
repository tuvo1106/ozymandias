# ADR-0027: Collectors emit metrics, not series; built-in collectors can be off

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

M3 §3 specified the collector interface as `Collect(ctx, emit func(Series))`
and the built-in collectors (host, docker, self) as always on. Building the
framework (PR #34) showed both to be the wrong shape:

- A wire `Series` carries a timestamp, a type, an interval, and the host
  and global tags. Every one of those is the scheduler's business, not the
  collector's: the timestamp is the run's, the interval is the scheduler's,
  and the tags must be added by the same rule statsd series get
  (`agenttags.Decorate`) or the two paths land in different series. Letting
  each collector build them is five chances per collector to get them wrong.
- "Always on" leaves no way to keep a collector from reading the real
  machine in a test, or to stop the Docker collector on a machine without
  Docker, where it would log a connection failure forever.
- The agent's own metrics ("self") are not read on a timer from a source;
  they are the self-metrics registry, which the agent already forwards with
  every aggregator flush. A collector for them would send them twice.

## Decision

- `Collect(ctx, emit func(collector.Metric))`: a metric is a name, a kind
  (gauge, rate, count), a value and the collector's own tags. The scheduler
  stamps, types and decorates it.
- Built-in collectors have `enabled` switches, on by default.
- Agent self-metrics stay in the self-metrics registry, extended with the
  collector framework's own (`ozy.agent.collector.*`) and Go runtime metrics
  (`ozy.runtime.*`).

The spec (docs/plan/M3-query-dashboards.md §3) is amended to match.

## Alternatives considered

| Option | Why not |
|---|---|
| `emit func(Series)`, as specified | Every collector builds timestamps, types and host/global tags itself, and can build them differently from statsd; the scheduler could not guarantee one series identity per thing measured |
| Always-on built-ins | Tests would read the real machine and its timers would count among a fake clock's waiters; a machine without Docker would log a failure every run with no way to stop it |
| A `self` collector | The self-metrics are already forwarded every flush; a second path would duplicate them |

## Consequences

- A collector is small: it reads and emits. The Docker collector and the
  checks (PRs 2 and 3) are written against `Metric`.
- Kinds are the scheduler's to map onto the wire, which is what let
  ADR-0026 (rates as gauges) be a one-line change.
- A collector cannot choose its own point timestamp. None needs to: a
  reading's time is the run's, to the second.
