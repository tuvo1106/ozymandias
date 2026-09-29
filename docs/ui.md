# The web UI

What each page does, how its URL works, and what to read when a widget shows
something other than a chart. The UI is served by `ozyd` itself at
`http://localhost:9400/` — the built assets are embedded in the binary, so there
is nothing to deploy beside it (`make web` builds them, `make build` embeds
whatever is there, `make dev` runs Vite on `:9401` against a native `ozyd`).

The definition a dashboard *is* — the JSON, field by field — is
[dashboards.md](dashboards.md). The endpoints behind every page are
[api.md](api.md). This document is the reader's side.

> Screenshots are not in the repo yet. They are part of M3's docs deliverable;
> the pages below are described so the text stands on its own without them.

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

Write a [metricql](query-language.md) query, run it, chart it. The box is the
dashboard editor's query box (§4): it completes aggregators, functions and
modifiers, and metrics and tag keys and values from what the store actually
holds, and it underlines a parse error as you type.

**Run** — or Ctrl+Enter (⌘+Enter on a Mac) — is what charts it. The URL holds
the query that was *run* (`?q=`), not what is in the box, so a link is always a
query and the chart that answers it, and back steps through queries rather
than keystrokes ([ADR-0022](adr/0022-the-explorer-runs-a-query-and-saves-through-the-editor.md)).
While the box holds an edit that has not been run, the page says so and the
chart is still the URL's query. Plain Enter is a newline: a query can span
lines.

Below the chart, a table of every line with its last, average, min and max over
the window — a dash for a line that had no value at all, which is not zero.
The chart area says which kind of nothing it is showing: no query yet;
running; **refused** (the query's fault — the server's message says why, and
running it again will not change it); **not answered** (out of time, ozyd
down — running it again might); answered with no series; or, while a new
query runs, the previous query's chart, dimmed and named. A refresh that fails
keeps the last answer on screen with the time it is from.

A link from M1 (`?metric=…&filter=…&by=…&agg=…`) still works: its parameters
are sent to ozyd as they are, and the query ozyd says they mean replaces them
in the URL and the box. If ozyd refuses them, the page says why in ozyd's
words and leaves the link as it was. **Clear** empties the chart and the
box — Run never does: a blank box runs nothing.

**Save to dashboard** picks a stored dashboard (or a new one) and opens it in
the editor with a timeseries widget for the charted query added and selected,
*not yet saved* — you see where it lands, and save it like any edit. It saves
the query that is charted, not an unrun edit in the box, and says so when the
two differ. A provisioned dashboard is listed but cannot be chosen: an edit to
it would be undone at the next restart.

### Metric Summary (`/metrics/summary`)

Every metric by how many series it has, highest first — the page for "why is
this slow, and why is the disk filling". A series is one distinct set of tags,
and each costs storage and query time, so a metric tagged with something
unbounded (a user id, a full URL, a timestamp) shows up here long before it
shows up as an outage.

Choose a metric to see its tag keys, most values first, each with how many of
the metric's series carry it. The key at the top is almost always the reason:
`route` with 40 values is a route template; `path` with 40,000 is a URL that
should have been one. **Chart it** opens the metric in the Explorer.

Counts are of what the store holds, until retention drops it — not of the last
hour — so a metric that was fixed yesterday still shows yesterday's number for
a while ([ADR-0023](adr/0023-series-counts-come-from-the-index.md)). `?prefix=`
filters the list and `?metric=` is the chosen one, so a link shows both. The
list is the 200 highest; when there are more, it says how many, and a prefix
narrows it.

Each part says which kind of nothing it has: counting; the counts could not be
read; an empty store; no metric under this prefix; a metric whose series carry
no tags; and a metric the store does not have, which is a different answer
from the one before it. While a new prefix is counted, the previous list stays,
dimmed, and says which prefix it is for.

### Dashboards (`/dashboards`)

Two lists, because they are two different kinds of thing:

- **Saved** — the stored dashboards. One marked *from file* was provisioned from
  `provisioning.paths` at startup; editing it through the UI would be undone at
  the next restart, so edit the file — or save a copy and edit that. A row the server could not read is named
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

**New dashboard** opens the editor on a blank definition.

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

Every widget has the same four ways of having nothing to draw, and each says
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
- **No query yet** — every query the widget has is blank, so nothing was
  asked. Not "No data": that would say the service is silent when nobody has
  asked it anything, which is what every new widget in the editor looks like.

A widget whose `type` this build does not know says so in its own frame rather
than taking the page down with it — which is what an older UI and a newer
`ozyd` look like, and is worth seeing as one broken square instead of one
broken application. A `reducer` it does not know is the same case one level
down: the widget names the reducer it cannot apply and shows nothing for that
query — a dash in a table's cell, a missing row in a toplist. It says so rather
than dropping the row silently, because a toplist one row short reads as a
complete ranking, and it says it *instead of* "No data", because a widget that
had series and could not reduce them is not a service that has stopped
reporting.

Changing the time range or a variable **keeps the last answer on screen** and
dims it until the new one lands, so the page fades rather than empties. A
background auto-refresh does not dim: it is asking the same question again, and
a page that blinked every ten seconds would be worse than one that said
nothing.

A warning that **every widget carries** — a template's `$env` resolving to no
filter is the usual one — is said once above the grid instead of in each
widget. Only once every widget has answered: until then nobody knows it is
shared, and a line that appeared and then moved back into the widgets would be
the page changing its mind in front of you. A widget whose every query was
refused is left out of the comparison, because a refusal carries no warnings
to agree or disagree with.

A `table` colours its cells by the widget's `conditional_formats`, cell by
cell, exactly as a `query_value` colours its number.

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

## 4. Editing a dashboard (`/dashboards/{id}/edit`, `/dashboards/new`)

**Edit** on a stored dashboard opens it in the editor; **Save a copy** on a
provisioned dashboard or a service's template instance opens a copy of it
(without its `uid`, which belongs to the original). `/dashboards/new?copy={id}`
and `/dashboards/new?service={name}&template={template_id}` are those links —
the source is in the URL, so a reload still knows what it was copying.

Either route also takes `?add={query}`, which is what the Metrics Explorer's
*Save to dashboard* links to: the editor opens with a timeseries widget for
that query added below everything, selected and unsaved. On a stored
dashboard's editor the parameter is removed once read, so a reload after
saving does not add the widget twice (a reload *before* saving drops it, and
the page asks before unloading, as for any unsaved edit). On `/dashboards/new`
it stays, since there it is the seed.

The page is the dashboard, live, with a panel beside it:

- **The grid.** Drag a widget by the bar at its top, resize it from its
  bottom-right corner. Both handles also take the arrow keys. Dropping a widget
  on another pushes the other one down (never sideways, never up), and a
  widget can never leave the twelve columns.
- **The preview is real.** It is drawn by the same widgets from the same batch
  as the dashboard page, so what you see is what the saved dashboard shows. It
  asks once typing pauses; a widget whose queries have changed since then is
  dimmed and says *Updating preview…*, and an answer is only ever drawn under
  the query text it answers.
- **The widget panel** offers exactly the fields its type uses, per
  [dashboards.md](dashboards.md). What it *carries* besides is listed too: a
  field the type does not use (left behind by a type change, say) is shown with
  the reason the server will refuse it and a *Remove* button; a key this build
  does not read is listed as kept; a value this build does not know — a
  reducer from a newer `ozyd` — is shown as itself, not as the first option of
  the list. Nothing is dropped or defaulted for you, including a new toplist's
  reducer: which number it ranks by is the question the widget answers.
- **The query box** completes aggregators, functions and modifiers from the
  parser's own vocabulary ([ADR-0021](adr/0021-the-query-editors-vocabulary-is-generated.md)),
  metrics and tag keys and values from the store, and `$variables` from the
  dashboard; Ctrl+Space opens the list where it would not open itself. Under
  it, ozyd's verdict on exactly the text in the box: *Parses* (with a
  *Format* button when the canonical spelling differs), the parse error with
  the offending character underlined, *Checking…* while that text has not been
  answered, or *Could not check* when ozyd could not be asked — which is never
  shown as either a pass or a fail.
- **A number field** that holds something that is not a number yet (`-`,
  `1.5` where a whole number is needed) says so and what the dashboard still
  holds meanwhile; blank means the field is absent, which for precision is
  "automatic" and not zero.
- **JSON** exports the definition alone — without the database's `id`,
  `provisioned` and timestamps, which the API refuses on the way in — and
  imports pasted text or a file. An import shows what it found (not JSON, not
  a dashboard and why, or what it will leave out) before it replaces anything.

**Saving** says which of these happened: saved; refused, with every problem
the server named; a conflict in the server's own words (the dashboard is
provisioned, or its `uid` is taken); the dashboard was deleted meanwhile (the
draft is kept, and can be saved as new); or no answer at all — which does not
mean nothing was written, since an answer can be lost after the write, so the
page says to check the list before creating again. A save the server accepted
but whose answer could not be read is reported as saved, and *Create* is then
disabled, because pressing it again would make a second dashboard.

## 5. Not built yet

Named here so the gap is visible rather than surprising: a heatmap for a
`dist:` query in the Metrics Explorer (it says to use `/api/v1/query/sketch`
instead), series counts over time ("top growing") on the Metric Summary,
drag-to-zoom on a chart, the Home overview dashboard, and a
Playwright run of the editor in CI. Sections beyond Metrics and Dashboards
belong to later milestones — [PLAN.md](../PLAN.md) has the map.
