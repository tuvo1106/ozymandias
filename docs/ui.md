# The web UI

What each page does, how its URL works, and what to read when a widget shows
something other than a chart. The UI is served by `ozyd` itself at
`http://localhost:9400/` — the built assets are embedded in the binary, so there
is nothing to deploy beside it (`make web` builds them, `make build` embeds
whatever is there, `make dev` runs Vite on `:9401` against a native `ozyd`).

The definition a dashboard *is* — the JSON, field by field — is
[dashboards.md](dashboards.md). The endpoints behind every page are
[api.md](api.md). This document is the reader's side.

> Screenshots are not in the repo yet. They are part of M3's docs deliverable
> and will land with the dashboard editor; the pages below are described so the
> text stands on its own without them.

## 1. The URL is the state

Every view is a link. What you are looking at — the metric, the filter, the time
range, each template variable, whether it is auto-refreshing — is in the query
string, so a URL pasted into an incident channel shows the other person exactly
what you were looking at.

That has one consequence worth knowing: the time range is stored as **what you
asked for**, not as the window it resolved to. `?range=1h` is "the last hour"
and is re-resolved against the clock on every refresh; `?from=…&to=…` is a fixed
window and does not move. A relative range is what you want in a link to "the
problem is happening now"; a fixed one is what you want in a link to "here is
what happened at 03:14".

Three states a variable can be in, and they are different on purpose:

| URL | Means |
|---|---|
| no `var.env` at all | the definition's own `default` |
| `var.env=` | explicitly every value — no filter on that tag |
| `var.env=prod` | that value |

The middle one is why clearing a selector does not simply fall back to the
default: "show me all environments" is a choice, and a link has to be able to
carry it.

## 2. Sections

The sidebar lists every section from day one, including the ones later
milestones build — a section whose milestone has not landed says so rather than
404ing, so the shape of the finished product is visible early. Live today:
**Metrics** and **Dashboards**.

### Metrics Explorer (`/metrics/explorer`)

Pick a metric, filter by tags, group by a tag key, chart it. The metric name and
the tag keys and values all autocomplete from what the store actually holds
(`/api/v1/metrics`, `/api/v1/tags`, `/api/v1/tags/values`), so the pickers offer
what exists rather than what you remember. Everything you choose goes in the
URL.

The text query editor and "save to dashboard" are not built yet; until then the
explorer builds its query from the pickers.

### Dashboards (`/dashboards`)

Two lists, because they are two different kinds of thing:

- **Saved** — the stored dashboards. One marked *from file* was provisioned from
  `provisioning.paths` at startup; editing it through the UI would be undone at
  the next restart, so edit the file. A row the server could not read is named
  rather than hidden, because a dashboard that has silently vanished from a list
  looks deleted.
- **Services** — every service a *template* dashboard covers. A template is not
  listed as itself: it has no data of its own, only the instances below it. The
  list is discovered from the tag index over the metrics the templates query
  ([ADR-0020](adr/0020-services-come-from-the-tag-index.md)), which means a
  service is offered when the store still holds one of those metrics for it —
  not "seen in the last day", and not every service that ever reported
  anything. If it says the list is partial, the server stopped before covering
  everything and the reason is in its log.

### One dashboard (`/dashboards/{id}`, `/dashboards/service/{name}`)

The same page either way. The second URL instantiates every template for one
service on the server and draws the result; nothing is stored, which is why
there is no id — a newly onboarded app has an overview before anyone writes
JSON.

Above the grid: one selector per template variable, one time picker, one
auto-refresh toggle. All three are the whole page's, not a widget's — a
dashboard is a set of things looked at *together*, and a widget with its own
time range is a question nobody asked. Auto-refresh is disabled for a fixed
window, because a window that does not move has nothing to refresh.

Charts on one dashboard **share a crosshair**: hovering any of them puts the
cursor at the same instant on all of them, the heatmap included. Only the time
axis is shared — dragging a percentage chart onto a byte chart's scale would be
worse than no sharing at all.

A dashboard is **one request** for its line charts, however many widgets it has
(`POST /api/v1/query/batch`), plus one per heatmap. Queries sharing a selector
are selected and bucketized once for the whole batch
([ADR-0018](adr/0018-shared-selection-in-a-batch.md)), which is the saving the
endpoint exists for; a dashboard past 50 queries is split into several batches
and loses some of it, which is an argument for smaller dashboards.

## 3. Widgets, and what they show when they cannot draw

| Widget | Draws |
|---|---|
| `timeseries` | a line per series, from one or more queries |
| `query_value` | one number, reduced over the window, coloured by the definition's `conditional_formats` |
| `toplist` | the groups ranked by their reduced value |
| `table` | one row per group, one column per query, lined up on the group |
| `heatmap` | the distribution behind a metric: a column per bucket, a band per bin, coloured by how many observations fell in it |
| `note` | the author's text |

Every widget has the same three ways of having nothing to draw, and each says
which one it is:

- **an error** — the query was refused and the message says why. Per widget on
  purpose ([ADR-0017](adr/0017-batch-queries-report-per-query.md)): one typo must
  not blank the other eleven. And per *query* within a widget: a chart with two
  queries, one of which has a typo, draws the line that answered and puts the
  message above it.
- **warnings** — it answered, and something about the answer is worth knowing: a
  variable that resolved to nothing, a series cap hit, a heatmap's error bar.
  Shown beside the data, never instead of it.
- **No data** — it answered with no series at all. That is a legitimate picture
  of a service that is not reporting, and saying so is the difference between it
  and a widget that failed silently. Nothing at all, rather than "No data",
  means the answer has not arrived yet.

A widget whose `type` this build does not know says so in its own frame rather
than taking the page down with it — which is what an older UI and a newer
`ozyd` look like, and is worth seeing as one broken square instead of one
broken application. A `reducer` it does not know is the same case one level
down: that query is left out and the widget says which reducer it could not
apply, because a toplist that had quietly dropped the row would read as a
complete ranking.

Changing the time range or a variable **keeps the last answer on screen** and
dims it until the new one lands, so the page fades rather than empties. A
background auto-refresh does not dim: it is asking the same question again, and
a page that blinked every ten seconds would be worse than one that said
nothing.

A `note`'s markdown is rendered as **text, not HTML**. Anyone who can `POST` a
dashboard can write one, and turning stored text into markup is how a monitoring
page becomes a way to run script in an operator's browser. Paragraphs and line
breaks survive; anything richer waits for a sanitizer chosen on purpose.

### Reading a heatmap

Darker is fewer, brighter is more, and nothing drawn means nothing was observed
in that value band in that bucket — a gap is real. The colour scale is
**logarithmic**: traffic is heavily skewed, and on a linear ramp everything but
the mode is the same colour, which is exactly the tail the widget exists to
show.

Three things the widget tells you in words, because the picture cannot:

- **"Bands are accurate to ±x%"** — a sketch stores a *shape*, not the
  observations. A band's position is within that much of the truth (α =
  (γ-1)/(γ+1), worst bucket on the chart). Count, sum, min and max are exact.
- **"Showing … only — n other groups matched"** — a `dist:` query with a `by`
  returns several distributions, and two drawn over each other are unreadable.
  Use one widget each.
- **"n observations at or below zero, which a log axis cannot show"** — see the
  axis note in [dashboards.md](dashboards.md#yaxis).

## 4. Not built yet

Named here so the gap is visible rather than surprising: the 12-column
drag/resize editor, the widget editor, the query editor with autocomplete and
inline parse errors, JSON import/export, "save to dashboard" from the explorer,
the Metric Summary (cardinality) page, drag-to-zoom on a chart, and the Home
overview dashboard. Sections beyond Metrics and Dashboards belong to later
milestones — [PLAN.md](../PLAN.md) has the map.
