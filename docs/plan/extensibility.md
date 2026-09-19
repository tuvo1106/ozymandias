# Extensibility (applies to every milestone)

app-node, app-python and app-ruby are the *first* users, not the only ones.
app-ruby (Ruby — no ozymandias SDK) is the standing real-world test of this doc:
it is onboarded with configuration, container labels and a stock OpenTelemetry
SDK only (`integrations.md` §3). ozymandias
must be adoptable by an app it has never heard of — in a language it has no SDK
for — without changing ozymandias's code. This doc is binding in the same way as
`testing.md` and `documentation.md`.

## 1. Rules

1. **No app-specific code in the core.** Nothing under `cmd/`, `internal/`,
   `pkg/`, `sdk/` or `web/src/` may mention app-node, app-python or app-ruby, or
   assume their routes, log formats, container names or metric names. CI greps
   for it (`scripts/check-no-app-coupling.sh`).
   App-specific material lives only in: `deploy/dashboards/<app>.json`,
   `deploy/monitors/<app>.json`, `deploy/agent.d/<app>.yaml`,
   `docs/plan/integrations.md`, and the apps' own repos.
2. **Behaviour an app needs is configuration, not a code branch.** E.g. the
   `judge-*` container-name rewrite (M3) and the `token=` redaction (M4) are
   generic, configurable rules that ship in `deploy/agent.d/app-python.yaml` —
   not built-in defaults.
3. **Protocols are the primary public interface; SDKs are conveniences.** Any
   process that can send a UDP datagram or an HTTP POST can use ozymandias.
4. **Public surfaces are versioned and stable** (§4).
5. **Every extension point is documented with a worked example and has a test
   that exercises it from the outside** (i.e. using only the public surface).

## 2. Ways in — what a new app can use, by effort

| Path | Works for | Available |
|---|---|---|
| **Any extended StatsD client** → agent :8125/udp | Any language with a statsd/extended StatsD library (Go, Java, Ruby, PHP, Rust, …) | M1 |
| **Log file or container stdout** → agent tailer, configured in `agent.d/*.yaml` or by container labels | Anything that logs | M4 |
| **OpenMetrics/Prometheus scrape** check → agent pulls `GET /metrics` | Anything already exposing Prometheus metrics (most infra, many apps) | M3 |
| **Raw HTTP** to the agent (`/v1/traces`) or intake (`/v1/series`, `/v1/logs`) per `docs/wire-protocol.md` | Any language; shell scripts and cron jobs via `curl` | M1/M4/M5 |
| **ozymandias SDK** (Python, Node) — statsd client, tracer, framework integrations | Python/Node apps | M1 / M5 |
| **OTLP/HTTP** receiver on the agent | Any language with an OpenTelemetry SDK | M8 |

### extended StatsD compatibility (M1) — a stated guarantee
The datagram format in wire-protocol §A is a compatible subset of extended StatsD:
standard metric types, `@rate`, `#tags`, `|T` timestamps; unknown `|x:` fields
(e.g. `|c:<container-id>`) are ignored; `_e{…}` events and `_sc` service checks
are accepted and counted but dropped until/unless implemented. Golden datagrams
captured from real third-party clients (Go `datadog-go`, Python `datadog`,
Node `hot-shots`) are committed under `pkg/wire/testdata/statsd-compat/`
and must parse.

## 3. Extension points inside ozymandias

| Extension point | Interface / mechanism | Spec'd in | Example shipped |
|---|---|---|---|
| Agent **checks** (pull-based collectors) | `collector.Check` interface + registry; instances configured in `agent.d/<name>.yaml`; **autodiscovery** from container labels `ozy.check.<name>.*` | M3 | `openmetrics`, `http_check`, `redis`, `postgres`, `process` |
| Agent **log sources + pipelines** | YAML: sources, grok patterns, remappers, redaction, exclusion; per-container via labels `ozy.logs.source`, `ozy.logs.service`, `ozy.logs.multiline_start` | M4 | `winston`, `python`, `uvicorn`, `caddy`, `postgres`, `redis`, `nginx`, generic `json` |
| **SDK integrations** | Public `Integration` protocol (`name`, `patch()`, `unpatch()`, `is_available()`), registered via `ozy.register_integration()`; built only on the public tracer/statsd API — integrations shipped in the SDK get no private hooks | M5 | see §5 |
| **Notifiers** | `notify.Notifier` interface + registry keyed by `type`; the generic `webhook` notifier with a templated body covers anything with an HTTP API | M6 | discord, webhook, email, log |
| **Dashboards & monitors as code** | JSON provisioning by `uid` from a directory (`provisioning.paths: […]`, several allowed, so an app repo can mount its own) + full CRUD API | M3 / M6 | home, self, two apps |
| **Widgets** | Widget registry in `web/src/widgets/` (type → component + JSON schema + editor) | M3 | per `ui.md` |
| **Stores** | `MetricStore`, `LogStore`, `TraceStore`, `SketchStore`, `queue.Queue` interfaces — alternative backends are possible without touching callers | M1–M7 | naive + real |
| **Query functions** | metricql function registry (`name`, arity, impl) | M3 | abs, top, moving_avg, … |

## 4. Stability and versioning

- **Wire protocol:** `/v1`. Within v1, changes are additive only: new optional
  fields, new endpoints. Receivers ignore unknown fields (tested). A breaking
  change means `/v2` served alongside `/v1`.
- **SDKs:** SemVer, independent of the server version. The SDK ↔ agent
  compatibility promise: any SDK 0.x.y works with any agent that serves `/v1`.
  Deprecations warn for one minor version before removal.
- **HTTP API `/api/v1`:** same additive rule; response goldens (testing.md L7)
  make breaks visible in review.
- **Config files:** unknown keys are an error (catches typos) but renamed keys
  keep a deprecated alias for one minor version.
- **On-disk formats:** versioned headers + forever-readable goldens (already
  required by testing.md L7).
- Each public surface has a reference doc; `CHANGELOG.md` entries flag
  anything that touches one with **[protocol]**, **[sdk]**, **[api]**, **[config]**.

## 5. SDK scope for generic use

Framework coverage is chosen so that a typical Python or Node web app works
without custom glue. app-node's and app-python's needs are a subset.

| | Python | Node |
|---|---|---|
| HTTP server | **ASGI** middleware (FastAPI, Starlette, Django-ASGI, Litestar), **WSGI** middleware (Flask, Django) | plain `http` wrapper, **Express/Connect** middleware, **Fastify** plugin, **Next.js** route-handler wrapper `withTelemetry(handler)` (app-node's `handle()` calls this — it is not special-cased) |
| HTTP client | httpx, `requests`/urllib3 | global `fetch`, `http.request` |
| DB | SQLAlchemy (covers Postgres/MySQL/SQLite), asyncpg, psycopg, sqlite3 | better-sqlite3, `pg`, generic `wrapQuery(fn)` helper |
| Cache / queue | redis-py, **arq**, Celery (stretch) | ioredis/`redis`, BullMQ (stretch) |
| Logging | stdlib `logging` filter + JSON formatter, structlog processor | winston format, pino mixin |

Anything marked stretch ships only if wanted; the point is that the
`Integration` protocol makes each of these a small, isolated, independently
testable module, and that **"writing your own integration"** in
`docs/sdk/*.md` is a real, tested walkthrough.

**Packaging:** both SDKs are built to be publishable from day one — proper
metadata, README, LICENSE, type hints/`.d.ts`, no path assumptions. Vendoring
into the owner's two apps is just one install method; `docs/sdk/*.md` documents
install from a registry, from git (`pip install "git+https://…#subdirectory=sdk/python"`),
and from a built artifact. `scripts/release-sdk.sh` takes target directories as
arguments; nothing in it names an app. (Private for now, so nothing is published; before any first publish, check PyPI/npm name availability for
`ozymandias` before first publish; fall back to a scoped npm name.)

## 6. Onboarding a new app — the acceptance test for this whole doc

`examples/` holds small third-party-style apps that use **only public
surfaces**, run in CI as part of the smoke suite:

| Example | Proves |
|---|---|
| `examples/flask-app` | Python SDK on WSGI + `requests` + sqlite3, none of which the owner's apps use |
| `examples/express-app` | Node SDK on Express + `pg`-less generic `wrapQuery` |
| `examples/go-app` | **No ozymandias SDK at all**: off-the-shelf extended StatsD client + JSON logs on stdout + container-label autodiscovery + hand-rolled trace POST via `net/http` |
| `examples/cron-script.sh` | `curl`/`nc` only: a metric, a log line, a span |
| `examples/otel-app` (M8) | Stock OpenTelemetry SDK → OTLP receiver |

`docs/onboarding.md` — "Instrument a new app in 15 minutes": pick a path from
§2, set four env vars or three container labels, get the auto-generated service
dashboard (below), add a monitor.

**Zero-config value:** any service that sends request spans or the standard
`http.request.*` metrics gets a generated **service overview** (RED + logs +
containers) from a dashboard *template* parameterised by `$service` — a new
app has a useful page before anyone writes dashboard JSON.

## 7. Milestone M8 — Open it up

Most of this doc lands inside M1–M7. What remains is its own milestone:

1. **OTLP/HTTP receiver** on the agent (`:4318`). **Both encodings are
   required**: JSON, and **protobuf** — OpenTelemetry Ruby (app-ruby) exports
   HTTP/protobuf only, and it is most SDKs' default. Either hand-write a
   minimal protobuf wire decoder for the OTLP messages (varints, tags,
   length-delimited fields — a worthwhile exercise; ask the owner) or add
   `google.golang.org/protobuf` + generated OTLP types to the allowlist; ADR
   either way. Mapping:
   `/v1/traces` → span mapping (OTel `SpanKind`/status/attributes → `type`,
   `error`, `meta`; resource attrs `service.name`, `deployment.environment`,
   `service.version` → unified tags; 128-bit ids map 1:1; nanoseconds →
   microseconds); `/v1/metrics` → gauges/sums → series, histograms →
   DDSketch (document the lossy bucket→sketch conversion);
   `/v1/logs` → log records with trace correlation. W3C `traceparent`
   accepted and emitted by the ozymandias SDKs alongside the native headers so
   mixed fleets produce one trace.
2. `examples/otel-app` + the rest of `examples/` wired into CI smoke.
3. SDK publishing pipeline (tag → build → publish) — **dry-run only** while the
   project is private; flipping it on is an owner decision.
4. `docs/onboarding.md`, `docs/extending.md` (one section per extension point
   in §3, each with a worked example), compatibility matrix.
5. Per-app API keys (from M7) surfaced in onboarding; per-key usage page.

**Test plan:** OTLP mapping tables (L1) with payload goldens captured from real
OTel SDKs (L7); fuzz the OTLP decoders (L4); mixed-propagation test — OTel
service calls ozymandias-SDK service and back, one trace (L8); every `examples/`
app in smoke (L9); "unknown field" tolerance tests on every `/v1` endpoint;
`check-no-app-coupling.sh` in CI.

**Acceptance:** app-ruby shows one trace from a Rails request through a
Sidekiq job with Postgres and Redis spans, correlated with its logs, using only
stock OpenTelemetry gems — and no `customer_phone` value anywhere in ozymandias
(scan test); each example app shows metrics, logs and traces with no
ozymandias code change; the Go example does so with no ozymandias SDK; a fresh
reader can follow `docs/onboarding.md` end to end (owner tries it on a third
project of theirs).

## 8. Explicit non-goals
Multi-tenant organisations/RBAC, a plugin binary ABI (extension = compile-in
registry or config, not dynamic loading), custom agent checks in an embedded
scripting language, SDKs beyond Python and Node (other languages use
extended StatsD, HTTP or OTLP).
