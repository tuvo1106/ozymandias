# Web UI and dashboards — the end state

The UI is built incrementally (each milestone spec lists its pages); this doc
describes the finished product so the pieces are designed to fit together from
the start.

## 1. Stack and structure

Vite + React + TypeScript + Tailwind, TanStack Query for server state, uPlot
for timeseries (canvas, handles 100k points), hand-rolled canvas for the flame
graph and service map, React Router. Dev on :9401 (proxy to :9400); production
build embedded in `ozyd` with `go:embed`. Light + dark themes. No
component library — a small in-repo `web/src/ui/` kit (Button, Select, Popover,
Table, Tabs, Toast, Drawer).

```
web/src/
  app/            routes, layout shell, nav, global time context
  pages/          home, metrics, dashboards, logs, apm, monitors, infrastructure, settings
  widgets/        timeseries, query-value, toplist, heatmap, table, log-stream, monitor-summary, service-summary, note
  query-editor/   metricql + logql editors (tokenizer, autocomplete, inline errors)
  charts/         uPlot wrapper, shared crosshair/sync, event overlays, threshold bands
  lib/            api client, time-range math, query builders, unit formatting, flame-graph layout, url state   ← 70% coverage gate
  ui/             primitives
```

Cross-cutting behaviour:
- **Global time range** (relative presets, absolute picker, drag-to-zoom on any
  chart, live/paused, auto-refresh) held in the URL; every page and widget
  obeys it. All view state is URL-encoded so any view is shareable.
- **Correlation everywhere:** from any chart point → "view logs / traces /
  hosts for this scope and time"; from a log → its trace; from a span → its
  logs and the host/container metrics at that moment.
- Units and formatting from metric metadata (`ms`, `bytes`, `%`, `req/s`).
- Empty, loading, error and partial-result (`truncated`) states designed for
  every data view.
- Keyboard: `/` focuses search, `t` opens the time picker, `g d`/`g l`/`g a`
  navigate.

## 2. Pages (milestone that introduces each)

| Page | Contents | From |
|---|---|---|
| **Home / Overview** | The landing dashboard: per-service health cards (req/s, error %, p95 with sparklines, status dot from monitors), triggered monitors list, recent error logs stream, ingest health strip (agent up, lag, drops), quick links to each app's dashboard. Built from the same widget system as any dashboard, provisioned as `deploy/dashboards/home.json` | M3 (metrics cards) → completed M6 |
| Metrics Explorer | query editor, chart, group-by legend table, save to dashboard | M1 → M3 |
| Metric Summary | metric list, type/unit, tag keys, series counts, top-growing | M3 → M7 |
| **Dashboards** | list, view, edit (12-col drag/resize grid), template variables, JSON import/export, provisioning from git, TV/fullscreen mode, per-widget time override | M3 |
| Infrastructure | host list + container map (tiles sized by memory, coloured by CPU, grouped by compose project), container detail with its metrics + logs | M3 → M4 |
| Log Explorer | search, histogram, facets, virtualized list, detail panel, live tail, saved views | M4 |
| APM | service list, service page, resource page, trace search, trace view (flame graph + waterfall + logs tab), service map | M5 |
| Monitors | list, editor with preview/backtest, detail with state timeline + events, mute | M6 |
| Settings | API keys, notifiers status + test, retention, pipeline/self-health, about/version | M7 |

## 3. Dashboard system (the core of "a dashboard")

- **Definition** = JSON document (schema in M3 spec §2), stored in SQLite,
  provisioned from `deploy/dashboards/*.json` by `uid`.
- **Widget types and their data source:**

| Widget | Source | Added |
|---|---|---|
| `timeseries` (line / area / bars, multiple queries, right axis, event overlay, threshold markers) | metricql | M3 |
| `query_value` (big number, conditional colour, sparkline background) | metricql + reducer | M3 |
| `toplist` | metricql + reducer | M3 |
| `table` (group rows × several queries as columns) | metricql | M3 |
| `note` (markdown) | — | M3 |
| `heatmap` / `distribution` (from DDSketch bins over time) | sketches | M3 |
| `log_stream` | logql | M4 |
| `log_timeseries` (count by facet) | logs aggregate | M4 |
| `service_summary` (RED for one service) | trace metrics | M5 |
| `trace_list` | trace search | M5 |
| `monitor_summary` / `alert_value` | monitors | M6 |

- A `POST /api/v1/query/batch` endpoint evaluates all of a dashboard's metric
  queries in one request with shared series selection (added in M3).
- Shared crosshair and zoom across all timeseries widgets on a dashboard.

### Dashboards shipped in the repo

| File | Shows |
|---|---|
| `home.json` | Overview described above |
| `app-python.json` | API RED by route; auth (logins ok/failed/locked, signups); submissions/min + verdict breakdown by language; arq queue depth, enqueue rate, queue wait p95, job duration by function; judge run p50/p95 by language + sandbox exits by code + OOMs; Postgres/Redis/api/worker container CPU+mem; worker error log stream; slowest resources table; triggered monitors |
| `app-node.json` | request rate/latency/errors by route; SQLite query p95 + top statements (M5); cover upload + sharp resize durations; Metron calls + burst budget remaining; data dir / covers / DB size; process host metrics; error log stream |
| `app-ruby.json` | mirrors its Grafana board from scraped metrics: §10.4 headline (small-order p90 wait vs large-order rate); wait percentiles by size class; ETA signed-error distribution; queue depth; breaches/remakes; Rails req/latency by controller#action; Sidekiq queues; containers; log stream |
| `ozymandias-self.json` | statsd packets in/dropped/parse errors; aggregator contexts; forwarder queue/retries/drops; intake req/s + rejects by reason; queue lag per topic (M7); TSDB head series, samples/s, WAL fsync p99, blocks, compaction time, bytes/sample; log ingest lines/s + bytes on disk; spans/s received vs kept; query latency; monitor eval lag |

## 4. Testing and docs
Per `testing.md` L11: `web/src/lib` ≥ 70% coverage (time math, query
tokenizer/autocomplete context, flame-graph layout, unit formatting, URL state
round-trip); React Testing Library for the editors and time picker; one
Playwright happy path per page against the smoke stack; screenshot goldens for
the chart wrapper in both themes (optional). Docs: `docs/ui.md` user guide with
screenshots per page, dashboard JSON schema reference, "build your first
dashboard" walkthrough; JSDoc on every exported component/hook.
