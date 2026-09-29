# ADR-0022: The Explorer charts a query it was told to run, and saves through the dashboard editor

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

M3 upgrades the Metrics Explorer from M1's pickers (metric, filters, group-by,
aggregator) to the text query editor the dashboard editor already has, and adds
"Save to dashboard". Two questions come with that, and both have a plausible
answer that is wrong here.

**When does the chart change?** The dashboard editor's preview follows the
draft once typing pauses. The Explorer's state is the URL (docs/plan/ui.md §1),
and a URL that followed the draft would either push a history entry per pause —
back steps through half-typed queries — or replace in place, and back no longer
returns to the previous query at all. Either way a pasted link could be a query
nobody finished.

**Where does saving happen?** The Explorer could `PUT` the chosen dashboard
itself. But saving already has a path, in the dashboard editor, that reports
each way a save goes — refused with every problem, a conflict in the server's
words, deleted meanwhile, no answer, an answer it could not read — and that
took two review rounds to get right. A second path would either repeat all of
that or report less, and it would put a widget on a dashboard at a place the
author never saw.

## Decision

- The URL holds `q`, the query that was **run**. The query box holds a draft;
  **Run** (or Ctrl/⌘+Enter) writes the draft to the URL, which is what charts
  it. Plain Enter stays a newline. While the draft differs from `q` the page
  says so, and the chart is still `q`'s.
- **Save to dashboard** opens the dashboard editor at
  `/dashboards/{id}/edit?add={q}` or `/dashboards/new?add={q}`. The editor
  opens with a timeseries widget charting `q` added below everything, selected
  and *unsaved*; the author saves it through the editor's one save. It saves
  the *charted* query, not the draft, and says so when they differ.
- On `/edit`, `add` is removed from the URL once read, so a reload after
  saving does not add the widget twice. On `/new` it stays: it is the seed, like
  `?copy=`, and nothing has been saved yet to add it to twice.
- M1 links (`?metric=…&filter=…&by=…&agg=…`) are translated to `q` on read, the
  same translation the server makes for those parameters, so an old link
  charts what it charted and shows it as text. The pickers are removed.

## Alternatives considered

| Option | Why not |
|---|---|
| Chart the draft once typing pauses, as the dashboard preview does | The preview is not in the URL; the Explorer is. Pushing per pause fills history with half-queries, replacing loses "back to the previous query", and either way a link can name a query nobody finished. The editor's parse verdict already answers "is this valid?" as you type, which is most of what a live chart would give. |
| `PUT` from the Explorer | A second save path with a smaller set of answers, and a widget placed where the author did not see it. Opening the editor costs one more click and reuses everything. |
| Keep the pickers beside the text box | Two editors of one query means translating in both directions, and the text language says things (`by`, functions, arithmetic) the pickers cannot, so the pickers would have to refuse to show some queries. The box completes metrics, tag keys and values, which is what the pickers were for. |
| Answer `?add=` in router state instead of the URL | Router state is gone on reload, so `/new` would reload as a blank dashboard — the same reason `?copy=` is in the URL. |

## Consequences

- A link to the Explorer is always a query and the chart that answers it.
  Typing changes nothing until Run, which is one keystroke more than a live
  chart.
- Saving from the Explorer gets every save state the editor has, and every
  future improvement to them, for free. The widget lands where the editor puts
  new widgets (below everything); the author can move it before saving.
- On `/edit`, reloading before saving drops the added widget. The page warns
  before unloading with unsaved changes, as it does for any edit.
- `?add=` is a public URL shape now — anything can link to the editor with a
  query to add — so it is documented in docs/ui.md.
