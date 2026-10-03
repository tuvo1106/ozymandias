# The dashboard definition

The JSON a dashboard *is*. This document is normative: the rules below are what
`internal/dashboard` implements, and a change to one is a change to the other in
the same commit. The HTTP around it is in [api.md](api.md#dashboards).

A definition describes what to draw. It contains no data, no time range and no
resolved variables — those are per-request, because two people look at the same
dashboard for different environments at the same time.

## A complete example

```json
{
  "uid": "checkout-overview",
  "title": "Checkout",
  "description": "the money path",
  "template_vars": [
    {"name": "env", "tag": "env", "default": "*"}
  ],
  "widgets": [
    {"id": "req", "type": "timeseries", "title": "req/s by route",
     "layout": {"x": 0, "y": 0, "w": 8, "h": 3},
     "queries": [{"q": "sum:http.request.count{$env} by {route}.as_rate()", "display": "line"}],
     "yaxis": {"min": 0, "unit": "req/s"}},

    {"id": "errors", "type": "query_value", "title": "5xx rate",
     "layout": {"x": 8, "y": 0, "w": 4, "h": 3},
     "queries": [{"q": "sum:http.request.count{$env,status:5*}.as_rate()", "reducer": "avg"}],
     "precision": 2,
     "conditional_formats": [{"op": ">", "value": 1, "color": "red"},
                             {"op": ">", "value": 0, "color": "yellow"}]},

    {"id": "slow", "type": "toplist", "title": "slowest routes",
     "layout": {"x": 0, "y": 3, "w": 6, "h": 3},
     "queries": [{"q": "p95:http.request.duration{$env} by {route}", "reducer": "max"}],
     "limit": 10},

    {"id": "latency", "type": "heatmap", "title": "latency distribution",
     "layout": {"x": 6, "y": 3, "w": 6, "h": 3},
     "queries": [{"q": "dist:http.request.duration{$env}"}]},

    {"id": "runbook", "type": "note",
     "layout": {"x": 0, "y": 6, "w": 12, "h": 1},
     "markdown": "Paging runbook: …"}
  ]
}
```

## Top level

| Field | Required | Meaning |
|---|---|---|
| `title` | yes | At most 200 bytes |
| `uid` | for provisioned | Stable identity: letters, digits, `-`, `_`, at most 64. It goes in a URL |
| `description` | no | At most 2000 bytes |
| `template_vars` | no | Up to 10 selectors |
| `template` | no | See [Templates](#templates) |
| `widgets` | yes | 1…100 |

**Why `uid` and not just `id`.** The numeric `id` is assigned by whichever
database inserted the row first, so two environments disagree about it. A `uid`
is chosen by the author and does not move, which is what makes a file in git
survive being edited — provisioning upserts by it. Definitions created through
the API may omit it.

## Template variables

```json
{"name": "env", "tag": "env", "default": "*"}
```

`name` is what queries write as `$name`; it must match the grammar's identifier
rule (a letter, then letters, digits, `_` or `.`) because that is what the lexer
reads a `$name` as. `tag` is the tag key the selector offers values of, and is
separate from `name` so a dashboard can call something `env` while it is stored
as `deployment_environment` — renaming a label in one place beats rewriting
every query. `default` is what is selected when nothing is in the URL; `*` or
empty means every value, which resolves to *no constraint* rather than to a
filter that happens to match everything.

**Every `$var` a widget's query mentions must be declared here.** This is the
one rule that cannot be checked by looking at a single widget, and it is the one
most worth having: without it a dashboard stores cleanly and then shows an error
in every chart, usually to somebody who did not write it.

**Variable names are case-insensitive.** The lexer lower-cases a `$name` as it
reads one, exactly as it does a tag key, and the API lower-cases the `var.<name>`
parameters and the `vars` object's keys to match — so declaring `Region` and
writing `$region` (or the reverse) is one variable at every layer. The
alternative was unpleasant: with folding in the validator but not the lexer, a
dashboard declaring `env` and querying `$Env` validates and *then* fails to
render on every widget, and nothing between the two is positioned to notice.

## Widgets

Common to all:

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | Unique in the dashboard. The UI uses it for per-widget URL state, so it must survive reordering — which an index would not |
| `type` | yes | One of the types below |
| `title` | no | At most 200 bytes |
| `layout` | yes | `{x, y, w, h}` in grid cells |

`layout` is in cells, not pixels, so a dashboard looks the same on a laptop and
a wall display. The grid is **12 columns** — it divides by 2, 3, 4 and 6, so
halves, thirds and quarters are all exact. `w` and `h` must be positive, `x` and
`y` cannot be negative, and `x + w` may not exceed 12: past the right edge is a
widget nobody can see, which stores happily and is then blamed on the browser.
`y` is unbounded — a dashboard may be as tall as you like.

### Types

| Type | Draws | Needs |
|---|---|---|
| `timeseries` | lines over time | ≥1 query; `display` optional, `line` if omitted |
| `query_value` | one number | ≥1 query with a `reducer` |
| `toplist` | ranked groups | ≥1 query with a `reducer`, optional `limit` |
| `table` | groups as rows, queries as columns | ≥1 query with a `reducer` |
| `heatmap` | a distribution's sketch bins over time | exactly 1 `dist:` query |
| `note` | markdown | `markdown`, and **no** queries |

**A note's text is rendered as text, not as HTML.** Anyone who can `POST` a
dashboard can write one, and turning stored text into markup is how a
monitoring page becomes a way to run script in an operator's browser. Line
breaks survive; link syntax does not become a link, it stays the characters
you typed — so write a URL plainly, as `deploy/dashboards/service.json` does.
The field is named `markdown` because that is what it will be once a sanitizer
has been chosen on purpose.

A field that belongs to another type is an error rather than ignored: a
`markdown` on a timeseries, a `limit` on anything but a toplist, a `precision`
on a chart. Ignoring it silently is how a dashboard ends up with a setting
nobody can find the effect of.

`heatmap` takes exactly one query, and its aggregator must be **`dist:`** — the
widget is drawn from [`/api/v1/query/sketch`](api.md#get-apiv1querysketch-post-apiv1querysketch),
which answers one `dist:` query and nothing else. A percentile there would be a
single number per bucket, which is a line and not a distribution, so it is
refused when the dashboard is saved rather than on every draw. The reverse is
refused too: `dist:` on any other widget type, since those are drawn from
`/api/v1/query`, which has no value for it.

Two distributions drawn over each other are not readable by anybody, so a
heatmap refuses a second query rather than draw something misleading — use two
widgets.

### Queries

```json
{"q": "sum:http.request.count{$env} by {route}.as_rate()", "display": "line", "name": "this week"}
```

| Field | Meaning |
|---|---|
| `q` | [metricql](query-language.md). Stored as text: it is what the author typed, what a diff shows, and what survives a change to the AST |
| `name` | Labels this query where a widget shows several — a table column header |
| `display` | `line`, `area`, `bars`, `points`. **timeseries only.** Optional — a query without one is drawn as `line` |
| `reducer` | `last`, `avg`, `sum`, `min`, `max`. Required by the one-number widgets, refused on `timeseries` and `heatmap` |

A `reducer` collapses a line into one number by reducing over **time**, across
the buckets of one line. The aggregation across *series* already happened inside
the query. A widget that shows one number per group has to say which number:
defaulting silently would make a chart and a toplist of the same query disagree
for a reason the reader cannot see.

Up to 10 queries per widget, so a chart can overlay this week on last week and a
table can put one query per column.

### `yaxis`

```json
{"min": 0, "max": 100, "unit": "%", "scale": "linear"}
```

timeseries and heatmap only. `min`/`max` are optional and may be `0` — that is
the common case ("start the axis at zero so a 2% wobble looks like 2%"). `min`
must be below `max`. `unit` is a label, not a conversion. `scale` is `linear`
(default) or `log`, and a `log` axis may not start at or below zero: a chart
whose axis silently clips its data is worse than one that refuses to be
configured.

**On a heatmap, set `"scale": "log"` unless you know you want otherwise.** A
sketch's bins are *geometric* — each is a fixed ratio wider than the one below
it — so a latency distribution from 4 ms to 2 s has hundreds of them, and on a
linear axis all but the slowest land in the bottom fraction of a percent of the
chart. The default stays `linear` because it is the default for every axis and a
widget that quietly ignored `scale` would be worse; the shipped
`deploy/dashboards/service.json` asks for `log`.

What a `log` axis does with the values it cannot place: a distribution's zero
bin is real (log γ 0 is undefined, so zero is stored as its own bin) and
negative observations are legal. Those are **left off the axis and counted**,
and the widget says how many. It does *not* fall back to a linear axis — one
zero-valued observation would then flatten a three-decade chart, silently, and
the reader would have no way to tell.

### `conditional_formats`

```json
[{"op": ">", "value": 1, "color": "red"}]
```

`query_value` and `table` only — a table colours each cell by them. In order;
the first match wins. `op` is one of `>`, `>=`, `<`, `<=`, `=`, `!=`.
`color` is a palette *name*, not CSS — a definition in git should not encode this
build's hex codes, and a palette that has to change for contrast should not
require editing every dashboard.

`precision` (0…10) is the decimal places a `query_value` or `table` shows. It is
optional, and `0` is a real answer — "round it to whole requests" — so absent
and zero are different.

## Templates

A definition with `"template": true` is not shown as itself. It is instantiated
once per service, so a newly onboarded app has a useful overview before anybody
writes JSON for it. `deploy/dashboards/service.json` is the one template ozymandias ships:
throughput, 5xx rate, p95 latency and a latency heatmap, all from the
`http.request.*` metrics every SDK sends.

A template **must declare a `service` template variable**, because that is what
it is instantiated over. Without the rule a `"template": true` dashboard stores
fine and then appears nowhere at all, which is the least debuggable outcome
available. The variable's `tag` is what the services are looked up under, so a
store that spells the label `service.name` is a one-line change to the template
rather than a rewrite of every query.

**Instantiation binds, it does not rewrite.** An instance is the same definition
with the `service` variable's `default` set to a service name; the queries still
say `$service` and the evaluator resolves them per request, exactly as it does
for `$env`. So the definition a reader sees and the query the server runs are the
same string, and there is no substitution pass in the package that promises not
to interpret data.

**The description is inherited word for word**, because it says what the
dashboard shows and that is the same for every instance. So write it about the
*content*, not about the templating: a description explaining that this is a
template, instantiated at `/dashboards/service/<name>`, tells the reader of an
instance that they are looking at a template — on the URL it just sent them to.
The shipped template said exactly that until a review caught it.

What else an instance differs by: `template` and `uid` are cleared — it is not
itself instantiable, and nothing stores it — and its title gains `": <service>"`,
so a picker showing four of them is a picker. An instance is a definition the API
would accept, which is what makes "save a copy of this" possible.

**A ratio is blank, not zero, when its numerator matches nothing.** This is worth
knowing before writing a template, because it decides which widget to use. An
empty selection produces *no series* — not a series of zeros — and no modifier
changes that: `.fill(zero)` fills empty buckets inside a series that exists. So
`sum:http.request.count{status:5*} / sum:http.request.count{*}` has nothing to
divide for a service with no 5xx, and the evaluator drops the group with a
warning saying so. As a `query_value` that renders as an empty square precisely
when the service is healthy, which reads as "broken". The shipped template
therefore draws its 5xx rate as a **timeseries**, where no line legibly means no
errors, and says so in the widget title.

You can write "0 when nothing matched" without a new modifier, by taking the
complement: `100 - sum:…{!status:5*} / sum:…{*} * 100`. A healthy service then
draws 0%. **Do not use it for an error rate.** When *every* request is a 5xx,
the complement's selection is empty, so the line vanishes at the moment the chart
matters most: a crash loop reads as "no data". The direct ratio draws 100 there.
That is why the shipped templates keep the direct ratio and its title, and
`TestEval_ComplementOfARatioIsZeroWhenNothingMatchesAndBlankWhenEverythingDoes`
pins both halves. A modifier that turns an empty selection into zero was
considered and not built: it would also turn a misspelt metric name into a
confident 0%.

**Which services exist** is `GET /api/v1/dashboards/services`: the values of the
`service` tag on the metrics the templates themselves query. It is not "seen in
the last day" — nothing here can answer that. See
[api.md](api.md#get-apiv1dashboardsservices) for the shape and
[ADR-0020](adr/0020-services-come-from-the-tag-index.md) for why.

## Limits

| Limit | Value |
|---|---|
| widgets | 100 |
| queries per widget | 10 |
| template variables | 10 |
| title | 200 bytes |
| description | 2000 bytes |
| a note's markdown | 16 KiB |
| the whole definition, over HTTP | 1 MiB |

These are not about taste — a 40-widget dashboard is somebody's problem but not
ours. They bound what one stored row can cost the server: a dashboard is a
single request to the query API, and every widget in it is queries the server
runs.

## What is *not* validated

Anything that depends on the data. A query that parses is valid even if the
metric does not exist, because the dashboard for a service is often written
before the service ships. A validator that ran queries would make saving a
dashboard depend on the data being there, which is the one property a definition
should not have.
