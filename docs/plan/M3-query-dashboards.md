# M3 — Query language, dashboards, infrastructure metrics

**Goal:** a tag-oriented text query language over the TSDB, saved dashboards,
host + container metrics from the agent, and app-python fully lit up with
metrics.

**Concepts learned:** lexing and recursive-descent parsing; the
select → time-aggregate → space-aggregate pipeline and why the order matters;
counter vs rate semantics; how agents discover and measure containers via
cgroups / the Docker API.

## 1. metricql — `internal/query/metricql`

### Grammar (EBNF; full version lives in `docs/query-language.md`)

```
expr      = term { ("+" | "-") term } ;
term      = factor { ("*" | "/") factor } ;
factor    = number | query | "(" expr ")" | func_call ;
func_call = ident "(" expr { "," arg } ")" ;            (* abs, log2, log10, clamp_min, clamp_max, timeshift, top, moving_avg … *)
query     = space_agg ":" metric "{" filter "}" [ "by" "{" key { "," key } "}" ] { "." modifier } ;
space_agg = "avg" | "sum" | "min" | "max" | "count" | "p50" | "p75" | "p90" | "p95" | "p99" ;
filter    = "*" | matcher { "," matcher } ;              (* comma = AND *)
matcher   = [ "!" ] key ":" value ;                      (* value may contain "*" *)
          | [ "!" ] key " IN (" value { "," value } ")" ;
          | "$" var_name ;                               (* dashboard template variable *)
modifier  = "rollup(" method [ "," seconds ] ")"         (* method: avg|sum|min|max|count|last *)
          | "as_rate()" | "as_count()"
          | "fill(" ("null" | "zero" | "last" | "linear") [ "," seconds ] ")" ;
```

Examples:

```
avg:http.request.duration{service:app-node,env:dev} by {route}
sum:http.request.count{service:app-python-api,status:5*}.as_rate() / sum:http.request.count{service:app-python-api}.as_rate() * 100
p95:judge.run.duration{language:python} by {problem_difficulty}.rollup(max, 60)
top(sum:container.cpu.usage{compose_project:app-python} by {container_name}, 5, "mean", "desc")
```

### Pipeline (evaluator)

1. **Parse** → AST (positions kept for error messages: `col 17: expected '}'`).
2. **Plan:** resolve template vars; choose `interval` = explicit rollup
   seconds, else `max(10, ceil_to_10((to-from)/300))`; choose default rollup
   method from metric type (gauge → avg, count/rate → sum, distribution →
   sketch merge); choose storage resolution (raw vs 5m rollups).
3. **Select** series (TSDB) for each query node.
4. **Time aggregation:** each series → aligned buckets
   `[from + i·interval)`, reduced by the rollup method. Empty bucket → null.
5. `as_rate()` → bucket value / interval; `as_count()` → per-bucket sum.
   Applies only to count/rate metrics; error otherwise.
6. **Space aggregation:** group series by `by` keys (missing key → group
   `"N/A"`); per bucket apply `space_agg` across the group, ignoring nulls
   (all-null → null). `pXX` on a distribution → merge sketches (per bucket,
   across the group) then quantile; `pXX` on a non-distribution → error.
7. **Fill**, then **functions**, then **arithmetic**: binary ops join series
   on identical group tag sets; scalar broadcast; division by zero → null.
8. Limits: ≤ 1000 selected series per query node (config; error tells the user
   to add a filter or group), ≤ 1500 buckets, 30s timeout via context.

### API

`GET|POST /api/v1/query?q=<expr>&from=&to=&interval=` — response as in M1
§3.4 plus `"query":<normalized text>`, `"warnings":[]`, per-series `"scope"`
(`route:/api/comics`) and `"expr_index"`. M1's structured endpoint is kept as a
thin adapter that builds a query string. `POST /api/v1/query/validate` →
`{ok, error:{msg, col}}` for the editor.

## 2. Dashboards

Metadata tables: `dashboards(id, title, description, definition_json, created_at, updated_at)`.

```json
{
  "title": "app-python overview",
  "template_vars": [{"name": "env", "tag": "env", "default": "*"}],
  "widgets": [
    {"id": "w1", "type": "timeseries", "title": "API req/s by route",
     "layout": {"x": 0, "y": 0, "w": 6, "h": 3},
     "queries": [{"q": "sum:http.request.count{service:app-python-api,$env} by {route}.as_rate()", "display": "line"}],
     "yaxis": {"min": 0, "unit": "req/s"}},
    {"id": "w2", "type": "query_value", "queries": [{"q": "…", "reducer": "last"}], "precision": 1,
     "conditional_formats": [{"op": ">", "value": 10, "color": "red"}]},
    {"id": "w3", "type": "toplist", "queries": [{"q": "…", "reducer": "avg"}], "limit": 10},
    {"id": "w4", "type": "note", "markdown": "…"}
  ]
}
```

- Widget types this milestone: `timeseries`, `query_value`, `toplist`, `table`
  (group rows × several queries as columns), `heatmap`/`distribution` (DDSketch
  bins over time, via `GET /api/v1/query/sketch?q=&from=&to=` returning merged
  bins per bucket), `note`. Later milestones add log, trace and monitor
  widgets — the full catalog and the end-state UI are in [`ui.md`](ui.md).
- `POST /api/v1/query/batch {queries:[…], from, to}` evaluates a whole
  dashboard in one request, sharing series selection between queries with the
  same selector.
- **Dashboard templates:** a definition with `"template": true` and a required
  `$service` variable is instantiated virtually for every service seen in the
  last day (`/dashboards/service/<name>`), so a newly onboarded app has a
  useful overview before anyone writes JSON. Provisioning reads every directory
  in `provisioning.paths`, so an app repo can mount its own dashboards.
- A first version of the **Home / Overview** dashboard (`deploy/dashboards/home.json`):
  per-service health cards + ingest health; completed in M6.
- CRUD: `GET/POST /api/v1/dashboards`, `GET/PUT/DELETE /api/v1/dashboards/{id}`;
  definition validated (queries must parse).
- `deploy/dashboards/*.json` are provisioned at startup (upsert by a stable
  `uid` field) so the app dashboards live in git.
- UI: 12-column grid (drag/resize), widget editor with the query editor
  (autocomplete for aggregator, metric, tag keys/values, modifiers; inline
  parse errors), template-variable bar, global time picker, auto-refresh,
  shared crosshair across charts, JSON import/export.
- Metrics Explorer upgraded to the text query editor; "Save to dashboard".
- **Metric Summary page:** list metrics with type, tag keys, and **series
  count per metric / per tag key** — the cardinality view.

## 3. Agent collectors — `internal/agent/collector`

`Collector` interface: `Name() string; Interval() time.Duration; Collect(ctx, emit func(Series)) error`.
Built-in collectors (host, docker, self) are always-on; **checks** are the
user-configurable kind: `Check` = a collector factory registered by name,
instantiated once per `instances:` entry in `deploy/agent.d/<name>.yaml`, or
via **autodiscovery** from container labels
(`ozy.check.redis.port=6379`, `ozy.check.openmetrics.path=/metrics`)
with `%%host%%` / `%%port%%` template variables resolved from the container.
A scheduler runs each on its interval (default 15s) with jitter and a timeout;
a failing collector is logged and counted, never fatal. Collector output goes
straight to the forwarder as gauges/rates (bypassing statsd aggregation).

- **host** (gopsutil): `system.cpu.{user,system,idle,iowait}` (%),
  `system.load.{1,5,15}`, `system.mem.{total,used,free,usable,pct_usable}`,
  `system.swap.*`, `system.disk.{total,used,free,in_use}` by `device`,
  `system.io.{r_s,w_s,rkb_s,wkb_s}` by `device`, `system.net.{bytes_rcvd,bytes_sent,packets_in.error,…}`
  by `interface` (rates computed from counter deltas), `system.uptime`.
  Note in docs: inside the Colima VM these describe the VM, not macOS; running
  the agent natively (`make dev`) reports the Mac.
- **docker** — raw HTTP over `/var/run/docker.sock` (no client library):
  `GET /containers/json` for discovery (refreshed every 10s and on
  `GET /events` stream `start`/`die`), `GET /containers/{id}/stats?stream=false`
  per container, bounded concurrency. Metrics: `container.cpu.usage` (% of one
  core: `cpu_delta/system_delta × online_cpus × 100`), `container.cpu.throttled`,
  `container.memory.{usage,limit,rss,cache}`, `container.net.{rx_bytes,tx_bytes}` (rates),
  `container.io.{read_bytes,write_bytes}` (rates), `container.pids`,
  `container.uptime`, plus `docker.containers.running` by `image_name`.
  Tags: `container_name`, `container_id` (short), `image_name`, `image_tag`,
  `compose_project`, `compose_service`, and `service:` from label
  `ozy.service` else `<compose_project>-<compose_service>`.
  **Short-lived containers** (app-python judge sandboxes live seconds): on a
  `die` event emit a final `container.lifetime` distribution point and
  `container.exits` count tagged `exit_code`, `oom_killed` — periodic polling
  alone would miss them. Judge containers are named `judge-*`: tag them
  `container_name:judge` (config `container_name_rewrite` regex rules — a generic mechanism; the
  `judge-*` rule itself ships in `deploy/agent.d/app-python.yaml`, not as a
  built-in) to avoid one series per sandbox.
- **checks shipped** (each generic, none app-specific):
  `openmetrics` (scrape a Prometheus `/metrics` endpoint: text-format parser
  written in-repo; counters → rates via delta between scrapes with **reset
  detection**; gauges as-is; histograms → by default `.bucket` (tag
  `upper_bound`), `.sum`, `.count` as counts, or with
  `histogram_buckets_as_distributions: true` each scrape's per-bucket deltas
  are interpolated into a DDSketch so `p90:` works uniformly — lossy, bounded
  by bucket width, and must handle **negative bounds** and `+Inf`; emits
  `openmetrics.up` and scrape duration per target; metric allow/deny + rename rules; label → tag mapping) — this
  one check makes most existing infrastructure and any Prometheus-instrumented
  app observable with zero code; `http_check` (status, latency, TLS expiry →
  `network.http.*`); `redis` (`INFO` over raw RESP: memory, clients, ops/s,
  keyspace hits/misses, evictions); `postgres` (connections, commits/rollbacks,
  tuples, cache hit ratio, table/index sizes — needs a driver: add `pgx` to the
  allowlist via ADR, or defer); `process` (by name/pid: cpu, rss, fds, threads).
  app-python's Redis and Postgres are configured through labels in its compose
  override — the first users of autodiscovery.
- **agent self:** goroutines, heap, statsd packets/parse errors/drops,
  forwarder queue bytes/retries/drops, collector durations.

## 4. Integrate app-python (metrics)
Per `docs/plan/integrations.md` §2 (M3 part): vendored wheel, `ozy.init`
in api + worker, ASGI metrics middleware, queue/judge/business metrics,
compose override. Ship `deploy/dashboards/app-python.json` and
`deploy/dashboards/app-node.json` and `deploy/dashboards/ozymandias-self.json`.

**app-ruby (integrations.md §3):** onboarded with a compose override + labels
only — the openmetrics check scrapes its existing yabeda `/metrics` on api and
worker. Ship `deploy/agent.d/app-ruby.yaml` and `deploy/dashboards/app-ruby.json`.
Add metricql functions `diff()` (for running-total gauges) and
`histogram_quantile(q, <bucket query> by {upper_bound,…})` (Prometheus-style
linear interpolation) — having both the bucket path and the sketch path lets
the notes compare their error on the same data.

## 5. Test plan

- **L1** lexer + parser tables (every production, every error with column);
  planner interval selection; each rollup method; `as_rate/as_count` type
  rules; grouping incl. missing keys; every space aggregator with nulls;
  fill modes; every function; binary-op join semantics (matching groups,
  scalar, mismatched groups dropped, div-by-zero); limits; template-var
  substitution; dashboard validation; provisioning upsert; docker stats math
  (CPU % from two canned stat payloads), label → tag mapping, name rewrite
  rules; rate-from-counter with counter reset.
- **L2** `parse(print(ast)) == ast`; evaluator: `sum by {k}` summed over groups
  == `sum` without `by`; bucket alignment invariant under shifting `from` by
  multiples of interval.
- **L3** differential: evaluator over tsdb == evaluator over naive store for
  random queries/data.
- **L4** fuzz: metricql parser; docker JSON decoders (`stats`, `containers`,
  `events`); OpenMetrics text parser; RESP reader.
- Checks: each against a fake server (canned `/metrics` pages from real
  exporters as goldens; scripted RESP; TLS expiry via `httptest` TLS server);
  autodiscovery label parsing + template-variable resolution; check instance
  lifecycle as containers start/stop; a failing/slow check never delays others.
- **L5** docker collector vs a fake Docker daemon (`httptest` on a unix
  socket): daemon down, slow, container disappears between list and stats,
  malformed JSON, event-stream disconnect + resume.
- **L7** API goldens for `/api/v1/query` (canned store → canned JSON);
  dashboard JSON schema goldens.
- **L8** in-process: provision dashboard → every widget query evaluates.
- **L9** smoke v3: start a throwaway container, assert
  `container.memory.usage{container_name:<x>}` appears, stop it, assert
  `container.exits` incremented.
- **L10** Python SDK ASGI middleware tests with a real Starlette/FastAPI test
  app: route pattern tag (not raw path), status tag, exception path, 404
  (`route:not_found`), streaming responses, no-op when disabled.
- **L11** UI: query-editor unit tests (autocomplete context detection, error
  rendering); time-range math; Playwright: build a widget, save, reload, see data.
- **L12** query latency: 1h/1d/7d ranges × 10/100/1000 series.

## 6. Docs deliverables
`docs/query-language.md` (EBNF, semantics of each stage, worked examples with
small numeric tables showing time- then space-aggregation); `docs/api.md`
(query, validate, dashboards); `docs/metrics-catalog.md` (system.*,
container.*, app-python metrics); DESIGN.md: query pipeline, collectors,
short-lived-container problem; `docs/diagrams/query-pipeline.mmd`; app-python
docs per its rules; `docs/sdk/python.md` ASGI section; `docs/notes/M3.md`.

## 7. Acceptance criteria

- [ ] All four example queries above evaluate correctly against live data.
- [ ] app-python dashboard shows: req/s + p95 latency by route, error %, arq queue depth, jobs/min by function, judge run p50/p95 by language, verdict breakdown, container CPU/mem for api/worker/postgres/redis, judge sandbox exits by exit code.
- [ ] app-ruby appears with **no change to its application code**: business and Rails/Sidekiq metrics charted; queue depth equals its Grafana board; small-order p90 wait agrees within bucket-interpolation error via both `histogram_quantile()` and the sketch path (both numbers reported). Restarting its api container does not produce a rate spike (counter-reset handling).
- [ ] Submitting a solution in app-python visibly moves queue depth, judge duration and `container.exits{container_name:judge}`.
- [ ] Query editor shows parse errors inline with the right column; autocomplete works for metric, tag key, tag value.
- [ ] Metric Summary shows per-metric series counts; no app-python/app-node metric exceeds 500 series after a normal session (cardinality discipline verified).
- [ ] app-python's own test suite + coverage gate pass with ozymandias absent.
- [ ] Tests and docs deliverables complete; `docs/notes/M3.md` has evidence.
