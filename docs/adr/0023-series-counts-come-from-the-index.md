# ADR-0023: Series counts come from the index, deduplicated by series key

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

M3's Metric Summary page lists "metrics with type, tag keys, and **series
count per metric / per tag key** — the cardinality view", and an acceptance
criterion reads it: no app metric may exceed 500 series.

`MetricStore` had nothing that counts. `MetricNames`, `TagKeys` and
`TagValues` answer "what exists", and the TSDB answers them by unioning
*strings* from the head and every block — which is enough for a list of names
and wrong for a count. One series is often in several places at once: a
series still being written after a block cut is in the block and the head,
and a compaction leaves a merged block beside its sources until they are
removed. `DB.Stats()` already shows what happens when that is summed: it adds
the head's series to every block's, so its `Series` counts a long-lived series
once per place it lives.

## Decision

Two methods on `MetricStore`, both answered from the index alone:

- `SeriesCounts(ctx, prefix)` — every metric with the prefix, with its number
  of distinct series.
- `TagCardinality(ctx, metric)` — for one metric, its series count and each
  tag key with how many of its series carry the key and how many distinct
  values it takes, all from one read. A bare tag counts toward the series and
  not the values, as `TagValues` already leaves the empty value out. The
  metric's count comes with the keys, not from a second call, because the page
  compares them ("all" series, "no tags" versus "no such metric") and two
  reads can straddle an append and disagree.

"Distinct" means by series key (`metric|k:v,…`). The TSDB gathers one
metric's series from the head and then every block — head first, for the
reason `Select` gives — into a map by key, so a series in three places is
one. The naive store's `series` table is already unique by key, so it is a
`GROUP BY` (two, in one read transaction, for `TagCardinality`).

`TestDB_MatchesTheNaiveStore` holds the two to the same answers,
and a targeted test holds the TSDB to one count for a series in both a block
and the head.

Like the other metadata methods they take no time range: a series counts
until retention drops it.

Two endpoints serve them — `GET /api/v1/metrics/cardinality` (highest first,
every metric counted and the response limited) and
`GET /api/v1/tags/cardinality?metric=` (most values first, with the metric's
own series count so "no tag keys" and "no such metric" are different
answers).

## Alternatives considered

| Option | Why not |
|---|---|
| Count through `Select` | Reads samples to answer a question about identities — a day of data per metric per page load, the amplification ADR-0020 was written against. |
| Sum each source's postings lengths | Cheap and wrong: it double-counts every series that lives in more than one place, which is the normal state of a busy one, and the double count is exactly the number this page exists to show. |
| Keep counters on the intake path | A write on the hot path and a second source of truth that has to be kept right across cuts, compaction, retention and restarts — all of which the index already is. |
| Counts over a window ("series seen in the last day") | Nothing indexes time per series; it would be a `Select` again. ADR-0020 took the same decision for the same reason. |
| An endpoint per metric only (no all-metrics count) | "Which metric is too big" is a question about all of them; a page that asks one at a time cannot rank. |

## Consequences

- Counting every metric walks the whole index: each series is resolved to its
  key once per source that holds it, and one metric's keys are held at a
  time. No sample is read. The blocks are acquired once per metric, since the
  head must be read before the block list is taken and reading the whole head
  first would hold a second copy of its identities; that costs about 240 ns
  per metric at 30 blocks. The `limit` does not bound the walk: ranking needs
  every count, and a prefix is what narrows it. That is the page's cost, and
  it is paid per load —
  there is no cache, as in ADR-0020.
- A series is counted until retention drops its block, so a metric that
  stopped exploding yesterday still shows yesterday's number for up to the
  retention period. The page says counts are of what the store holds, not of
  the last hour. The naive store never forgets at all, which the differential
  test sidesteps by disabling retention.
- Distributions' sketches live in the sketch store and are not counted here;
  a distribution's series show up as its `<name>.count`, `.sum`, `.min` and
  `.max` series, which have the same tags.
- The head keeps no empty series. It used to create and index a series
  before applying its samples, and three paths left one with nothing in it
  until the next truncation — every sample out of bounds, a failed log write,
  and replay restoring series whose samples a block had long since taken —
  where every reader of the head's index (the metadata queries, the
  per-metric series limit, and these counts) saw a series no query could.
  Append now refuses out-of-bounds samples before resolving, forgets any
  series it created and stored nothing in, and replay forgets the restored
  series it left empty. A series is visible, empty, only for the moment
  between its creation and its first sample inside one append.
- `DB.Stats().Series` still double-counts and feeds `ozy.store.series`. It is
  a size gauge, not a cardinality, and fixing it would put this walk on the
  self-metrics tick; it is left as it is and named here.
- M7's "top growing" needs counts over time, which this does not give; it is
  the upgrade path, not this ADR.
