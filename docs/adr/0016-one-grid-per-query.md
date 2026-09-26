# ADR-0016: One bucket grid per query, and `.rollup()` sets it

- **Status:** Accepted
- **Date:** 2026-09-26

## Context

metricql lets one query combine several selections:

```
sum:http.request.count{status:5*}.as_rate() / sum:http.request.count{*}.as_rate()
```

Arithmetic between two lines is pointwise, so both sides must be reduced onto
the same buckets. Usually they are: the planner picks one interval for the
request. But `.rollup(method, seconds)` names a bucket width *per query node*,
and the milestone spec ([`docs/plan/M3-query-dashboards.md`](../plan/M3-query-dashboards.md)
§1) says only "choose `interval` = explicit rollup seconds, else
`max(10, ceil_to_10((to-from)/300))`". It does not say what

```
sum:a{*}.rollup(avg, 60) + sum:b{*}.rollup(avg, 300)
```

means, nor what happens when the request also passes `interval=30`. Something
has to, because a dashboard will eventually contain one of these by accident.

## Decision

**Every node in one request is evaluated onto one grid, and conflicting
requests for that grid are an error.**

The interval is the first of these the request has:

1. the explicit `interval` parameter,
2. a `.rollup(_, seconds)` on any query node,
3. `DefaultInterval` — about 300 points, rounded up to a multiple of the
   agent's 10-second flush.

Two `.rollup()`s naming different widths is refused. A `.rollup()` that
disagrees with an explicit `interval` is refused. Both errors name the two
widths and where each came from.

`.rollup(method)` without a width stays purely a time-aggregation override and
never touches the grid, which is the common use.

## Alternatives considered

| Option | Why not |
|---|---|
| Resample: evaluate each node on its own width, then interpolate onto the result grid | Inventing values. Resampling a 300s sum onto 60s buckets has to either divide (asserting the rate was uniform, which is the thing a chart is being read to find out) or repeat (five identical points that look like real measurements). Both make up data, and the made-up data is indistinguishable from the measured kind once it is a line on a chart. |
| Take the coarsest width and re-aggregate the finer nodes onto it | Defensible, and it is what a later milestone may want. But it silently changes what one side of the expression means — a `.rollup(avg,60)` the user wrote is answered at 300s — and a query whose meaning depends on its neighbours is hard to reason about and harder to debug. Refusing costs one error message and teaches the rule once. |
| Let the last (or first) rollup win | Order-dependent semantics in a language where the operands of `+` are otherwise symmetric. |
| Ignore `interval` when a rollup is present, per the plan's literal wording | Then `interval=30` is silently dropped, and a monitor that pins its interval so its threshold means the same thing on every evaluation would not notice that it had stopped doing so. |

## Consequences

- The evaluator has one `grid` threaded through it, and no resampling code
  exists to be wrong. Every node's output is directly comparable with every
  other node's, which makes the join a map lookup.
- `timeshift` is the one thing that moves a grid, and it moves it *whole* — it
  requires the offset to be a multiple of the interval for the same reason, so
  that the two windows have the same shape.
- A rare query is refused that another system might have answered. The error
  says which two widths disagree, so the fix is to remove one.
- If resampling is ever wanted, this ADR is the one to supersede, and the
  decision is already isolated in `(*Evaluator).plan`.
