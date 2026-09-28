# ADR-0018: A batch shares selections through a bucketized cache

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

[`docs/plan/M3-query-dashboards.md`](../plan/M3-query-dashboards.md) §2 asks
`POST /api/v1/query/batch` to evaluate a dashboard "in one request, sharing
series selection between queries with the same selector". Without the sharing
the endpoint is a loop that saves HTTP round trips, which is worth something but
not what the plan asked for.

The queries of a real dashboard overlap heavily. A service overview asks
`sum:http.request.count{service:api} by {route}`, then `avg:…{service:api}`, then
`max:…{service:api} by {host}`: three questions, one selection of the same
thousand series, the same chunks decoded three times.

The obstacle is where the work lives. `tsdb.MetricStore.Select` returns a
streaming `SeriesSet` — refs plus sample iterators, closed when the query is
done — so "the selection" is not a value that can be handed to a second query.
Sharing it means either materializing something, or changing the store
interface that four implementations and the M2 differential tests agree on.

## Decision

**Split a query node in half and memoize the first half for the duration of one
batch.**

- *Select and reduce onto the grid* — walk the `SeriesSet`, apply the
  post-filter, bucketize each series with the rollup method — depends only on
  the selector, the post-filter, the grid and the method. This half is cached.
- *Group and aggregate* — `by {route}` versus `by {host}`, `sum` versus `avg` —
  is what differs between the queries that share a selection, and runs per query
  over the cached slices.

The cache key is those four things, written with `%q` so that no two selections
can spell one key. It lives in the request's `state` and dies with it; nothing is
shared between requests, so nothing can be stale.

Two consequences fall out and are deliberate:

- **The window and the interval belong to the batch, not to each query.** Two
  queries planned onto different grids share nothing, so per-query intervals
  would turn a dashboard into N unrelated requests with extra steps. A dashboard
  has one time picker (ADR-0016 already gives one grid per request).
- **One deadline for the batch.** Fifty queries at `DefaultTimeout` each is
  twenty-five minutes. A batch that runs out reports what it has and fails the
  rest with the deadline, per query (ADR-0017).

Memory is capped by `SelectionCacheBytes` (64 MiB), counted as the bucketized
values actually held. When the budget is gone the batch stops caching and keeps
answering: a slower dashboard is better than a failed one, and the queries that
already shared a selection keep the saving.

Measured on 1000 series × 6 dashboard-shaped queries: **4.9 ms shared versus
10.8 ms separate**, a 2.2× saving, and one `Select` of the store instead of six.

## Alternatives considered

| Option | Why not |
|---|---|
| Do not share; the endpoint just loops | The plan asks for sharing, and the round-trip saving alone does not justify a second query endpoint. |
| Add `Series(ctx, sel) []SeriesRef` to `MetricStore` and share the ref list | Shares the index lookup but not the chunk decoding, which is the larger cost. Four store implementations, the index and the M2 differential tests would all have to agree on a new method for the smaller half of the win. |
| Cache raw samples per series instead of bucketized values | Bigger (a week of ten-second samples is far more than 1500 buckets) and still has to be re-bucketized per query. The grid is shared anyway, so bucketizing once is the point. |
| Cache keyed on the query text | Misses the case the sharing exists for: two *different* queries over one selection. |
| Evaluate the batch's queries concurrently | A cache filled concurrently needs locking, and the queries that would contend are exactly the ones that would have hit the cache. Sequential first, with the numbers recorded; revisit if a dashboard of distinct selectors turns out to be the common shape. |
| A process-wide cache across requests | Stale data, invalidation against a live head block, and a memory budget shared by every caller. A per-request cache cannot serve anyone yesterday's numbers. |

## Consequences

- A dashboard of related queries costs roughly one selection instead of N.
  A dashboard of unrelated ones costs what it did before, plus a map lookup.
- `Evaluator.Eval` and `Evaluator.Batch` run the same code path — `Eval` passes a
  cache of its own that nothing hits — so a single query cannot drift from a
  query in a batch.
- Nothing in the cached half may be mutated by the grouping half. `accumulator`
  reads the values and keeps its own totals; a future modifier that writes in
  place would show up as another widget's numbers changing, which is why the
  invariant is stated at the call site and in `selectBucketed`.
- Refs are cloned on the way into the cache, because a store may reuse the tag
  slice it handed out for the next series.
- `GET /api/v1/query/sketch` (the heatmap endpoint, still to build) selects
  sketches rather than samples and shares nothing with this path.
