# ozymandias — master plan

An observability platform built to learn how these systems work underneath:
agent, ingestion, storage engines, tracing, query, dashboards, alerts.

Instrumented apps:

- `../app-node` — Next.js 16 + SQLite (better-sqlite3/Drizzle), single
  process, winston JSON logs to `data/logs/%DATE%.log`, runs on the host on :3939.
- `../app-python` — FastAPI api + arq worker (launches Docker sandbox jobs)
  + Postgres + Redis behind Caddy, docker-compose (and a k8s/KEDA path).
- `../app-ruby` — Rails 8.1 API + Sidekiq + Postgres + Redis + React, compose
  and a kind k8s cluster; already exports Prometheus metrics (yabeda) with a
  Grafana board. **Public repo.** Ruby — no ozymandias SDK exists for it, on
  purpose: it is the proof that the generic paths (scrape, stdout logs,
  OpenTelemetry) are enough.

Together they cover the three adoption styles: Node SDK, Python SDK, no SDK.

**This plan is written to be handed to an implementing model.** Read in this order:

1. This file — architecture, decisions, principles, milestone map.
2. [`AGENTS.md`](AGENTS.md) — working rules, definition of done, dependency allowlist.
3. [`docs/plan/testing.md`](docs/plan/testing.md) — coverage gates and the twelve test layers. Binding on every milestone.
4. [`docs/plan/documentation.md`](docs/plan/documentation.md) — the document set, code-doc standard, learning-notes template, CI enforcement. Binding on every milestone.
5. [`docs/plan/extensibility.md`](docs/plan/extensibility.md) — how apps ozymandias has never heard of plug in; extension points; stability rules; M8. Binding on every milestone.
6. [`docs/wire-protocol.md`](docs/wire-protocol.md) — every payload on every hop. Normative.
7. `docs/plan/M<n>-*.md` — one spec per milestone: tasks, interfaces, formats, test plan, docs deliverables, acceptance criteria.
8. `docs/private/integrations.md` (gitignored) — exactly where the three apps get touched.
9. [`docs/plan/ui.md`](docs/plan/ui.md) — the end-state web UI: pages, the dashboard/widget system, shipped dashboards.

---

## 1. Decisions (settled — do not re-litigate)

| Decision | Choice | Why |
|---|---|---|
| Backend language | Go (latest stable) | What Prometheus, Loki and production observability agents use — their source is the reference material. Good at UDP servers, byte-level storage, single-binary agent. |
| Wire protocol | Own protocol + own SDKs (Python, Node) | Writing the client side is where you learn what an SDK does (buffering, context propagation, patching). OTLP receiver is a stretch goal. |
| Deployment | Local docker-compose | No auth / TLS / multi-tenancy until M7. |
| Repo visibility | **Public** (ADR-0012) | Built as if public from the start (no secrets, ever); `SECURITY.md` and `CODE_OF_CONDUCT.md` ship with it. App-integration detail that names the owner's private apps stays in a gitignored `docs/private/`. |
| Scope | Metrics, logs, traces, query+dashboards+monitors, storage engines from scratch | All four, ordered as vertical slices. |
| Process shape | Two binaries: `agent`, `ozyd` (modular monolith) | Hard internal seams (`MetricStore`, `LogStore`, `TraceStore`); M7 cuts intake from storage with a queue. |
| TSDB | From scratch (WAL, Gorilla chunks, inverted index, blocks, compaction) | The core storage-engine exercise. |
| Log store | From scratch, Loki-style (label index + compressed chunks), then bloom filters | Teaches index-light vs index-heavy trade. |
| Trace + sketch store | Pebble (embedded LSM KV) | A third from-scratch engine has diminishing returns; the learning here is key design on a KV. |
| Metadata (dashboards, monitors, API keys, metric types) | SQLite via `modernc.org/sqlite` (pure Go) | Boring on purpose. |
| UI | Vite + React + TS + Tailwind + uPlot + TanStack Query, embedded into `ozyd` for prod | Matches app-node's stack. |
| SDK distribution | Publishable packages; for the owner's apps, built artifacts vendored (`vendor/*.whl`, `vendor/*.tgz`) | Works inside app-python's Docker build context and in both apps' CI with no registry; other users install from git or a registry. |
| Audience | Generic tool; app-node, app-python and app-ruby are the first users | No app-specific code in the core; extended-StatsD-compatible UDP, documented HTTP protocol, label-based autodiscovery, OpenMetrics scraping, and (M8) OTLP mean any app in any language can plug in. |
| Repo conventions | `the project template` | Conventional Commits (no `Co-Authored-By`), `feat/…` branches + one PR per feature, never commit to `main`, Keep-a-Changelog, ADR log, treat-as-public secrets posture. |

## 2. Ports

| Port | Owner | Purpose |
|---|---|---|
| 8125/udp | agent | extended StatsD-style metrics from SDKs |
| 8126/tcp | agent | Trace intake from SDKs (`POST /v1/traces`), agent `/healthz`, `/debug/vars` |
| 9400/tcp | ozyd | Intake (`/v1/*`), query API (`/api/v1/*`), embedded UI (`/`) |
| 9401/tcp | Vite dev server | UI in dev, proxies `/api` → 9400 |

(Chosen to avoid app-node :3939 and app-python :8000/:8080/:5173.)

## 3. Architecture

```
 app-node (host, npm run dev)        app-python (compose: api, worker, judge containers)
   ozymandias-node SDK                       ozymandias-py SDK
        │  statsd UDP :8125  ·  traces HTTP :8126  ·  log files / docker logs
        ▼
 ┌─────────────────────────── agent (Go, one per host) ───────────────────────────┐
 │ statsd server → aggregator (10s buckets)     host + container collectors    │
 │ trace receiver → stats concentrator (RED) → sampler      log tailer → pipeline │
 │ forwarder: batch · gzip · retry w/ backoff · bounded buffer · drop-oldest      │
 └──────────────────────────────────────┬─────────────────────────────────────────┘
                                        │ HTTP :9400  /v1/series /v1/sketches /v1/logs /v1/traces
                                        ▼
 ┌───────────────────────── ozyd (Go, modular monolith) ─────────────────────┐
 │ intake ──► MetricStore (tsdb)  SketchStore  LogStore  TraceStore               │
 │            ▲ query engine (metricql, logql)      ▲ monitor evaluator → notifiers│
 │ metadata: SQLite (dashboards, monitors, metric types, api keys, events)        │
 │ HTTP: /api/v1/query /logs /traces /dashboards /monitors /metrics /tags         │
 └──────────────────────────────────────┬─────────────────────────────────────────┘
                                        ▼
                         web UI (React, embedded via go:embed)
```

## 4. Principles (apply to every milestone)

1. **Tracer bullet first, then deepen.** Each store starts naive behind its
   interface and is replaced by the real engine. The naive one stays in the
   tree as the benchmark baseline and test oracle.
2. **Telemetry must never break the app.** SDKs: UDP fire-and-forget, bounded
   queues, drop-on-overflow, ≤2s timeouts, every exception swallowed and
   counted. No SDK code path may throw into app code.
3. **Absent, not broken.** `OZY_AGENT_HOST` unset → SDK is a total no-op
   (no sockets, no threads, no timers). Both apps' test suites and CI must pass
   with ozymandias not running.
4. **Unified tagging.** Every metric, log and span carries `service`, `env`,
   `version`, `host`. Cross-pillar correlation depends on it.
5. **Cardinality discipline.** Tag by route *pattern*, never by id, user, or
   raw path. SDK path normalizers enforce this.
6. **Dogfood.** Agent and ozyd emit their own metrics under `ozy.*`.
7. **Generic core.** Apps are configuration, never code branches (extensibility.md §1).
8. **Teach the why.** This is a learning project: package docs and comments
   explain mechanism and trade-off; each milestone produces `docs/notes/M<n>.md`.
9. **Injectable clock and filesystem roots everywhere** so storage, aggregator
   and monitor tests are deterministic.

## 5. Repo layout (target)

```
cmd/agent/            cmd/ozyd/          cmd/loadgen/
internal/agent/{config,statsd,aggregator,collector,tailer,logpipeline,tracerecv,concentrator,sampler,forwarder}
internal/intake/
internal/tsdb/{wal,chunkenc,head,index,block,compact,naive}
internal/sketch/                 DDSketch implementation (shared by agent + server)
internal/sketchstore/  internal/logstore/  internal/tracestore/
internal/query/{metricql,logql}
internal/monitor/{eval,state,notify}
internal/meta/                   SQLite metadata
internal/api/                    HTTP handlers for /api/v1
internal/selfmetrics/            ozy.* internal metrics
pkg/wire/                        payload structs + validation, shared by agent and ozyd
sdk/python/                      ozymandias-py   (package name: ozymandias)
sdk/node/                        ozymandias-node (package name: ozymandias)
web/                             UI
docs/{wire-protocol.md,plan/,notes/}
scripts/{smoke.sh,release-sdk.sh,chaos/}
deploy/{docker-compose.yml,agent.yaml,ozyd.yaml,agent.d/,dashboards/,monitors/}   the only place app-specific config lives
examples/                        third-party-style apps using only public surfaces (run in CI smoke)
.github/  CONTRIBUTING.md  CHANGELOG.md  docs/adr/  lefthook.yml      from the project template
```

## 6. Milestones

Sizes are relative (S ≈ a weekend, M ≈ 1–2 weeks of evenings, L ≈ 3+ weeks).
Do them in order; each ends with something visible in a real app.

| # | Spec | Size | Ends with |
|---|---|---|---|
| M0 | [Skeleton](docs/plan/M0-skeleton.md) | S | Both binaries build, compose up, CI green |
| M1 | [Metrics tracer bullet](docs/plan/M1-metrics-tracer-bullet.md) | S–M | app-node request rate + latency on a chart |
| M2 | [The real TSDB](docs/plan/M2-tsdb.md) | L | Naive store replaced; survives `kill -9`; p95 from DDSketch |
| M3 | [Query language, dashboards, infra metrics](docs/plan/M3-query-dashboards.md) | M | app-python dashboard (API RED, queue depth, judge durations, containers); app-ruby dashboard from scraped `/metrics` with zero app code, matching its Grafana board |
| M4 | [Logs](docs/plan/M4-logs.md) | M–L | Search + live tail over all three apps' logs (incl. app-ruby' plain-text Rails logs, no app change) |
| M5 | [Tracing / APM](docs/plan/M5-tracing.md) | L | One trace: `POST /submissions → arq → docker run judge → verdict` |
| M6 | [Monitors and alerting](docs/plan/M6-monitors.md) | M | Discord/webhook alert when judge queue backs up |
| M7 | [Pipeline hardening](docs/plan/M7-hardening.md) | M | Queue between intake and storage; chaos drills pass |
| M8 | [Open it up](docs/plan/extensibility.md#7-milestone-m8--open-it-up) | M | OTLP receiver; app-ruby traced end to end (Rails → Sidekiq) by stock OpenTelemetry Ruby; example apps onboard with zero ozymandias changes |

Each completed milestone cuts a release: M1 → `v0.1.0` … M8 → `v0.8.0`.

Stretch (unspecced): browser RUM SDK · continuous profiler ·
anomaly monitors · synthetic checks · agent as a k8s DaemonSet for app-python's
KEDA path.

## 7. Reading list (milestone order)

- extended StatsD datagram format docs; `DataDog/datadog-agent`: `comp/statsd`, `pkg/aggregator` (M1)
- Gorilla paper, Facebook, VLDB 2015 (M2)
- Fabian Reinartz, "Writing a Time Series Database from Scratch"; Ganesh Vernekar's Prometheus TSDB blog series (M2)
- DDSketch paper, Datadog, VLDB 2019 (M2)
- Grafana Loki architecture docs (M4)
- Dapper paper, Google 2010; `DataDog/dd-trace-py` integration patterns; W3C Trace Context (M5)

## 8. Owner decisions

**Settled**

- Go module path: `github.com/tuvo1106/ozymandias`.
- SDK distribution: **vendored artifacts** copied into the apps for now.
- **Nothing is published to PyPI/npm yet** (ADR-0014): the SDKs are pre-1.0 and
  the apps consume vendored artifacts. They are still *built* as publishable
  packages (extensibility.md §5); M8's publishing pipeline stays a CI dry-run
  until the SDK API is stable. `ozy` is free on PyPI; on npm it is taken, so a
  first publish there uses the scoped name `@tuvo1106/ozy`.

**Recommended defaults — proceed with these unless the owner says otherwise**

- *Milestone order:* **keep M2 (TSDB) before M4 (logs).** M4's log store is built
  on M2's `wal` package, and M3's percentiles (and app-ruby' histogram→sketch
  path) need M2's DDSketch — swapping means writing the WAL early under a
  weaker spec. Guard against the long-M2 slump instead: M2's build order makes
  each piece demoable on its own (`chunkenc` compression numbers → WAL crash
  demo → `tsdb inspect`), and DDSketch may be pulled to the *front* of M2 so
  real percentiles show up on the app-node chart in the first week.
- *M8 protobuf:* **hand-write a small decode-only wire reader**
  (`internal/protowire`: varint, fixed32/64, length-delimited, field tags) and
  hand-map the OTLP **trace and log** messages on top of it. It is ~a day of
  work, squarely in the spirit of the project, and highly fuzzable. Add
  `google.golang.org/protobuf` as a **test-only** dependency and use it as a
  differential oracle (encode with the official library → decode with ours),
  alongside goldens captured from real OTel SDKs. OTLP *metrics* (a much larger
  message family) stay JSON-only until something needs them — app-ruby'
  metrics arrive by scrape.
- *`postgres` check:* **allow `github.com/jackc/pgx/v5`, agent only**, via an ADR.
  Unlike Redis (RESP is ten lines), speaking Postgres by hand means the
  startup handshake plus SCRAM-SHA-256 auth — real work that teaches nothing
  about observability. Build it last in M3; it connects with a dedicated
  read-only `pg_monitor` user whose credentials come from env, never from
  labels or committed config.

**Still open**

- Which pieces the owner wants to hand-write rather than delegate (asked at the
  start of each milestone — AGENTS.md §1).
