# ADR-0026: Collector rates are sent as per-second gauges

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Most of what the host (and, next, Docker) exposes is cumulative: bytes
received since boot, reads since the disk appeared. The agent turns two
readings into a per-second rate at collection (`collector.Rates`, DESIGN.md
§12). The question is how that value travels and is stored.

The wire has a `rate` type, and the collector framework first sent these as
`rate` with the collector's interval. But the query engine rolls up `rate`
series the way it rolls up `count` series — it sums the points in each
bucket (`defaultRollup` in `internal/query/metricql/eval`), so that
`.as_rate()` means the same on both. For a per-second value that is wrong: a
bucket holding n points reads as n times the rate. A 1-day chart of steady
1000 B/s (290 s buckets, about 19 points each) showed about 19 000, and a
1-hour chart (20 s buckets, one or two points) swung between 1000 and 2000.
Review of PR #34 found it before any data was written this way.

## Decision

A collector's `Rate` is sent as a **gauge** whose value is per second.
`collector.Rate` stays as a kind in the framework, because it records what
the collector computed and keeps its tests honest, but the scheduler maps it
to `wire.KindGauge`, interval 0. The catalog lists these metrics as
"gauge, per second".

A gauge averages over a bucket, and the average of per-second values over a
bucket is the bucket's rate, at any width. Summing across devices or hosts
(`sum:… by {host}`) still gives total throughput.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep `rate` on the wire and make the engine average rates | Changes the meaning of every existing `rate` series and of `.as_rate()` / `.as_count()` on them, which M1's engine and docs define; a query-layer change for a collector's problem |
| Send each interval's delta as a `count` | Correct under the engine, and keeps totals (`.as_count()` over an hour). But names like `system.io.r_s` and `system.net.bytes_rcvd` promise a per-second value; a count would chart as bytes per bucket unless every query says `.as_rate()`, and Datadog's names, which these follow, are per-second gauges |
| Send the raw cumulative counter and compute `rate()` at query time (Prometheus) | Needs counter-reset handling in the engine and a new function; the plan puts the rate computation in the agent (M3 §3) |

## Consequences

- Charts of `system.io.*`, `system.net.*` and (next) `container.net.*`,
  `container.io.*` read the same at every zoom.
- `.as_rate()` and `.as_count()` are refused on them, as on any gauge. "Bytes
  in the last hour" is not directly available; `avg` × the window gives it.
- The wire's `rate` type remains, unused by the agent. Anything that sends
  one gets the engine's count-like rollup, as documented in
  docs/query-language.md.
