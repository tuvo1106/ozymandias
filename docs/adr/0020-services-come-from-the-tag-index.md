# ADR-0020: A template's services come from the tag index, not from a window

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

M3 §2 asks for dashboard templates: a definition with `"template": true` and a
`$service` variable, "instantiated virtually for every service seen in the last
day (`/dashboards/service/<name>`)".

Nothing in ozymandias can answer "seen in the last day" about a service.

- `internal/meta` records one row per *metric*: name, type, interval, first
  seen. There is no per-service row and no last-seen column anywhere, for
  metrics or for tag values.
- The metric store's tag index answers `TagValues(metric, key, limit)` — the
  values a key takes on a metric — and takes **no time range**. It is an index
  over the series that exist, and a series exists until retention drops it.
- The only time-bounded question the store does answer is `Select(selector,
  from, to)`, which is a *query*: it returns every matching series in the
  window, and a service list built from it would read a day of samples for
  every metric a template mentions on every page load.

So the deliverable — a working `/dashboards/service/<name>` and something for a
picker to list — needs a source for "which services are there", and the
window in the spec is the part that has no cheap implementation.

## Decision

**Services are discovered from the tag index, over the metrics the template's
own queries name.** For each stored template, `internal/api` walks its widgets'
queries for metric names, maps each to the series its tags live on (a
distribution's are on its `.count`), and unions `TagValues(series, tag,
1001)` — where `tag` is the `service` variable's own tag key.

The set is therefore **"services the store still holds one of this template's
metrics for"**, not "services seen in the last day". The window is dropped; the
M3 spec is amended to say so in the same PR.

Two consequences are accepted rather than worked around:

- A service that stopped reporting is offered until its last series falls out of
  retention. Its dashboard draws empty charts over the selected window, which is
  what any dashboard of a dead service does.
- A service that reports none of the template's metrics is *not* offered. This
  is deliberate: instantiating for it would produce a dashboard of empty charts
  with no explanation.

The union is capped at 1000 services — the same number as
`eval.MaxSeriesPerNode`, past which the template's own queries would be refused
for selecting too many series — and a capped response says `"truncated": true`
rather than looking complete. Discovery stops early once that cap is reached,
because there is nothing further to learn from asking.

## Alternatives considered

| Option | Why not |
|---|---|
| A `Select` per metric over the last day, taking the services from the returned series | Honours the spec exactly, and costs a day of samples read per metric per page load. The tag index exists so that "what values does this key take" does not have to read data; this would be the one endpoint that ignores it. Rejected on cost, not on correctness |
| Add `TagValues(metric, key, from, to, limit)` to `tsdb.MetricStore` | The right long-term answer, and four implementations plus the differential tests that hold them to each other — for one endpoint in one milestone. The same reasoning as ADR-0018's rejected store change. Left as the upgrade path: this decision is reversible behind the existing handler |
| Track `last_seen` per (metric, service) in `internal/meta` | A write on the intake hot path for every series in every payload, to answer a question one endpoint asks. It also invents a second source of truth about what exists, which can disagree with the store |
| Discover from every metric in the store, not just the template's | More services, all of them ones the template has nothing to say about. "Every service that has ever sent any metric" is a worse list than "every service this dashboard can draw" |
| Skip discovery: serve `/dashboards/service/<name>` for any name | Then a typo in a URL pasted into a runbook renders a grid of empty charts, which reads as "the service is down" rather than "the service is misspelt". Discovery is what makes the 404 possible |

## Consequences

- `internal/api.Dashboards` gains two dependencies (`Values TagValueReader`,
  `Types MetricTypes`). They are optional fields: without them the two template
  endpoints answer `503` with a reason, and dashboard CRUD is unaffected — which
  keeps CRUD tests free of a metric store.
- Cost per request is proportional to the number of *templates*, not to the
  number of dashboards: a row is checked for `"template": true` with a shallow
  decode before it is validated, because validating means parsing every query in
  a definition and most rows are somebody's ordinary dashboard. Fifty
  thousand-query dashboards, none of them templates, went from 25.0ms a request
  to 5.5ms (`BenchmarkDiscover`).
- Cost per request is also one tag-index lookup per distinct `(series, tag key)`
  pair — memoized, because templates are expected to overlap: the deployment
  shape provisioning is built for is several app repos each mounting a directory
  beside the stock one, and they all draw `http.request.count`. Asking once per
  template instead would spend the budget below on repeats and answer
  `truncated` with real services missing. `/dashboards/service/{name}` stops as
  soon as the name it was given turns up, so the common read is one lookup. And
  the number of templates is bounded by nothing — anybody who can `POST` a
  dashboard can mark one — so the lookups are capped at **500 per request** and a
  request that hits the cap answers `"truncated": true`. A count rather than a
  deadline: a 30-second budget like the query path's would make the list depend
  on how busy the machine is, and a service list that changes under load is not
  something a picker can be built on. There is no cache; if a polling picker
  shows up in a profile, a short-TTL cache is the next step and nothing above
  changes.
- `internal/dashboard` gains `Metrics()`, `ServiceTag()` and `Instantiate()`.
  Instantiation **binds** the service variable's default rather than rewriting
  `$service` in the query text, so the definition a reader sees and the query
  the server runs stay the same string, and variable resolution stays in the
  evaluator where it already is for every other variable.
- An instance is this build's **re-encoding** of the definition, not the
  author's bytes — the "verbatim" guarantee the CRUD endpoints make does not
  apply to it. That is why the response nests the definition under `dashboard`
  instead of splicing provenance beside it.
- When a time-bounded tag lookup does arrive (or rollups make one cheap), this
  endpoint is where the window goes back in, and the spec's original wording
  becomes implementable without an API change.
