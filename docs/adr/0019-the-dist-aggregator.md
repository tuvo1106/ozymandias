# ADR-0019: `dist:` is an aggregator, and its answer is not a number

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

[`docs/plan/M3-query-dashboards.md`](../plan/M3-query-dashboards.md) §2 specifies
the heatmap widget and its endpoint: "`heatmap`/`distribution` (DDSketch bins
over time, via `GET /api/v1/query/sketch?q=&from=&to=` returning merged bins per
bucket)". It does not say what `q` looks like, and metricql has no spelling for
it. The grammar requires an aggregator before the metric — `sum:x{*}`,
`p95:lat{*}` — and every existing one produces a number.

A distribution is not a number. It is the merged sketch itself, and the only
operation two of them support is merging. There is no avg-or-sum choice to make
about how a group's series combine, because merging is exact and is the only
thing sketches do.

So the endpoint needed a query spelling, and the three obvious ones were all
wrong in the same way: they say "give me a number" and then do not.

## Decision

**`dist` is a space aggregator, and the evaluator refuses it wherever a number
is expected.**

```
dist:http.request.latency{service:api} by {route}
```

- `GET|POST /api/v1/query/sketch` answers it, and *only* it: the expression must
  be exactly one `dist:` query. Not a sum of two, not a ratio — arithmetic
  between two distributions is a different and much larger question than this
  endpoint answers, and silently using the left one would be worse than
  refusing.
- `/api/v1/query` refuses `dist:` with a message naming `/api/v1/query/sketch`
  and the percentile aggregators, because those are the two things the person
  might have meant.
- `/api/v1/query/sketch` refuses a non-`dist:` query with a message naming
  `/api/v1/query`. Each endpoint knows where the other one is.

Everything else about the query is unchanged: the same filter, the same `by`,
the same template variables, the same window rules and the same grid. Moving
between a line chart and a heatmap should not mean rewriting the filter.

## Alternatives considered

| Option | Why not |
|---|---|
| Reuse a percentile spelling and ignore the quantile (`p95:lat{*}` on `/query/sketch`) | The response is not a p95. The canonical text echoed back would say one thing and the body another, and a dashboard would store a query whose text does not describe its widget. |
| Accept any aggregator and ignore it (`avg:lat{*}`) | Same problem, plus it makes `avg:` and `sum:` synonyms in one context and not in another. |
| A separate `q` grammar for this endpoint (a bare `lat{*}`) | Two grammars for one query language. The editor, the autocomplete, the validator and the printer would each need to know which one they are in. |
| A function call — `distribution(lat{*})` | Functions in metricql take and return *lines*; this one would take a selection and return something no other function can consume. An aggregator is the position in the grammar that already means "how a group's series combine". |
| Put it on `/api/v1/query` and return an empty `series` with the bins beside it | A response whose main field is always empty, and every client branching on which half is real. |

## Consequences

- The grammar gains one keyword. The printer, the parser and the validator need
  no other change, because an `Agg` is a string and `dist` is in the table with
  the rest.
- `dist:` is a query that parses everywhere and is answerable in one place. The
  refusals are therefore part of the contract, not an implementation detail, and
  each names the endpoint that does answer.
- The heatmap widget's query is an ordinary metricql query, so the query editor,
  autocomplete and template variables work on it with no special case.
- A later milestone wanting arithmetic on distributions (a difference of two
  latency shapes, say) has somewhere to put it, and will need its own decision
  about what that even means.
