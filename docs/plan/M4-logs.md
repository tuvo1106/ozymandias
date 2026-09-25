# M4 — Logs

**Goal:** the agent tails app-node's rotating winston files and app-python's
container stdout, a processing pipeline normalizes them, a from-scratch log
store indexes and compresses them, and the UI offers search, facets and live tail.

**Concepts learned:** reliable file tailing (offsets, rotation, truncation);
the Docker log stream format; at-least-once delivery with checkpoints; log
pipelines; index-everything (Elasticsearch) vs index-labels-only (Loki) and
where bloom filters sit between; why log volume is *the* cost problem.

## 1. Agent log collection — `internal/agent/tailer`

Config (`agent.yaml`):

```yaml
logs:
  enabled: true
  registry_path: /var/lib/ozymandias-agent/registry.json
  sources:
    - type: file
      path: /var/log/apps/app-node/*.log      # bind mount of ../app-node/data/logs
      service: app-node
      source: winston
      tags: [env:dev]
    - type: docker
      include_labels: ["com.docker.compose.project=app-python"]
      exclude_names: ["^judge-"]                  # sandboxes: stdout is user program output, not logs
      source: python
      multiline: {start_pattern: '^(DEBUG|INFO|WARNING|ERROR|CRITICAL|\{|\d{4}-\d{2}-\d{2})'}
```

### File tailer
- **Polling, not inotify** (bind mounts through Colima/Docker Desktop don't
  deliver fs events reliably): glob rescan every 10s; per-file `stat` + read
  every 1s (250 ms while active).
- Identity = `(device, inode)`; offset tracked per identity. New file matched by
  glob: start at **end** on first-ever discovery (`start_position: end|beginning`
  config), at offset 0 if it appeared after agent start (a rotation product).
- Rotation (winston-daily-rotate-file creates a new dated file): keep reading
  the old file until EOF and idle for 60s, then close. Rename-style rotation:
  inode follows the file; the path now has a new inode → new tailer.
- Truncation: `size < offset` → reset offset to 0, count it.
- Lines split on `\n`; max line 256 KiB (longer → truncated + flagged);
  partial last line held until newline or 5s idle.
- **Registry:** `{identity → {path, offset, last_seen}}` written atomically
  (temp + rename) every 1s when dirty and on shutdown. Offsets are committed
  **only after** the forwarder has a 2xx for the batch containing those lines
  → at-least-once; duplicates possible after a crash (document this; dedupe is
  a non-goal).

### Docker tailer
- Discovery shares the container watcher from M3.
- `GET /containers/{id}/logs?follow=1&stdout=1&stderr=1&timestamps=1&since=<last_ts>`.
  Demux the multiplexed stream: 8-byte frame header
  `[stream(1=stdout,2=stderr), 0,0,0, size uint32 BE]` then payload (TTY
  containers are raw — detect via inspect `Config.Tty`). Frame payloads can
  split lines; reassemble per stream.
- Registry stores last timestamp per container id; on reconnect use `since`
  and drop lines with `ts <= last`.
- Tags: same container tags as M3; `service` same rule; stderr lines get
  default status `error` only if no parser sets one (uvicorn logs to stderr at
  INFO — hence parsers first).

### Per-container configuration by label (autodiscovery)
Any container can opt in and configure itself without touching agent config:
`ozy.logs.enabled=true|false`, `ozy.logs.source=<pipeline>`,
`ozy.logs.service=<name>`, `ozy.logs.multiline_start=<regex>`,
`ozy.logs.tags=k:v,k2:v2`. `logs.container_collect_all: true` tails every
container with source auto-detected (JSON vs plain). This is how an app
ozymandias has never heard of gets its logs in.

### Multiline
Aggregator per source: a line not matching `start_pattern` is appended to the
previous event (Python tracebacks); flush on next start line or after 1s;
max 500 lines / 256 KiB per event.

## 2. Pipeline — `internal/agent/logpipeline`

Ordered processors, configured per `source` (built-in defaults for `winston`
and `python`, overridable in YAML):

1. **json parser** — if the line is a JSON object, it becomes `attrs`.
2. **grok-lite parser** — named-group regexes for non-JSON lines. Built-ins:
   Rails request logs (`[request-id] Started … / Processing by … / Completed 200 OK in 12ms (Views: … | ActiveRecord: …)` — grouped into one event per request id, the multiline aggregator keyed on a **correlation tag** rather than only on line shape); Sidekiq (`jid`, class, `elapsed`, `done|fail`);
   Python `%(levelname)s %(name)s %(message)s`; uvicorn access
   `INFO:     1.2.3.4:5 - "GET /path HTTP/1.1" 200 OK`; Caddy JSON; Postgres
   and Redis default formats.
3. **remappers** — message (`message|msg`), status (`level|levelname|severity`
   → `debug|info|warn|error|critical`; numeric syslog levels too), timestamp
   (`timestamp|time|ts`, RFC3339 or epoch; else receive time), service,
   trace ids (`trace_id|dd.trace_id`, `span_id`).
4. **reserved-field collision** — numeric app `status` → `attrs.status_code`.
5. **redaction** — regex rules applied to message and string attrs: JWTs
   (`eyJ…\.…\.…`), `Authorization:` values, emails (config, default on),
   `password|secret|token|api_key` keys → `[REDACTED]`. A default rule also redacts
   URL query values for `token|code|key|signature` params (first motivating
   case: app-python's worker prints verify/reset links with
   `EMAIL_BACKEND=console`). All rules are generic and configurable; per-app
   additions go in `deploy/agent.d/<app>.yaml`.
6. **sampling/exclusion** (optional): `exclude_at_match` regex (e.g. healthz
   access lines), per-source rate limit (default 1000 lines/s, excess dropped
   + counted).

Batches of ≤ 1000 logs / 2 MiB / 2s → forwarder → `POST /v1/logs`.

## 3. LogStore — `internal/logstore`

```go
type LogStore interface {
    Append(ctx context.Context, batch []wire.Log) error
    Search(ctx context.Context, q logql.Query, fromMs, toMs int64, opts SearchOpts) (*SearchResult, error) // newest-first default, cursor pagination
    Aggregate(ctx context.Context, q logql.Query, fromMs, toMs int64, a AggSpec) (*AggResult, error)      // count by time bucket and/or facet
    Facets(ctx context.Context, q logql.Query, fromMs, toMs int64, keys []string, limit int) (map[string][]FacetCount, error)
    Close() error
}
```

### v1 — Loki-style
- **Stream** = unique label set `{service, source, host, env, status}` (low
  cardinality by construction) → `streamID`. Stream catalog in SQLite:
  `log_streams(id, labels_json, key UNIQUE)`, `log_stream_labels(stream_id, k, v)`.
- **Write path:** `Append` → WAL (reuse `tsdb/wal`, own record type) → per-stream
  in-memory head block. A head block is sealed at 256 KiB raw or 5 min age:
  entries `ts varint-delta | len uvarint | bytes(json{message,attrs,tags,trace_id,span_id})`
  → zstd → appended to the stream's current chunk file.
- **Layout:** `data/logs/<YYYY-MM-DD>/<streamID>.chunk` =
  `magic|version` + repeated blocks `minTs|maxTs|n|rawLen|compLen|crc32c|zstd bytes`
  + footer block-index + footer offset. Catalog table
  `log_chunks(stream_id, day, path, min_ts, max_ts, bytes, entries)`.
  Sealed blocks are fsynced before the WAL is truncated.
- **Read path:** label matchers → streams (SQL) → chunks overlapping range →
  blocks overlapping range (footer index) → decompress → line filter →
  k-way merge across streams by timestamp. Newest-first reads blocks in
  reverse. Stop at `limit` (default 100); cursor = `(ts, streamID, block, idx)`.
  Bounded parallel block decoding; context-cancellable; per-query scan budget
  (default 2 GiB raw) → partial result with `"truncated": true`.
- **Retention:** delete day directories older than `logs.retention` (7d default).

### v2 — per-block bloom filters (second half of the milestone)
At seal time, tokenize message + string attr values (lowercase, split on
non-alphanumerics, keep tokens 3–40 chars) into a bloom filter (~1% FPR, size
from token count) stored beside each block. Free-text and `@attr:value` terms
consult it to skip blocks. Format version bump; v1 chunks remain readable.
Measure and report the skip rate and speedup — this *is* the lesson.

## 4. logql — `internal/query/logql`

```
query   = or_expr ;
or_expr = and_expr { "OR" and_expr } ;
and_expr= unary { ["AND"] unary } ;                 (* juxtaposition = AND *)
unary   = ["-" | "NOT"] atom ;
atom    = "(" query ")" | term ;
term    = key ":" value                              (* reserved: service, source, host, status, env, trace_id *)
        | "@" path ":" value                         (* attribute, dotted path *)
        | "@" path ":" (">"|">="|"<"|"<=") number
        | "@" path ":[" number " TO " number "]"
        | quoted_string | bare_word ;                (* free text, case-insensitive substring; "*" wildcards *)
```
Planner splits a query into **stream matchers** (index) and **line filters**
(scan). A top-level OR across different labels plans as a union of stream sets.

## 5. API + live tail

- `GET /api/v1/logs?q=&from=&to=&limit=&cursor=&order=desc|asc`
- `GET /api/v1/logs/aggregate?q=&from=&to=&interval=&by=status` → histogram above the list
- `GET /api/v1/logs/facets?q=&from=&to=&keys=service,status,@path`
- `GET /api/v1/logs/tail?q=` — **SSE**. Intake publishes accepted logs to an
  in-process hub; each subscriber has a filter (compiled logql) and a bounded
  channel (1000) — slow consumer → drop + periodic `event: dropped` notice.
  Heartbeat comment every 15s.
- Metric queries from logs are deferred to M6 (log monitors use `Aggregate`).

## 6. UI — Log Explorer
Search bar with logql autocomplete (keys, known values), time picker,
status-colored count histogram (drag to zoom), virtualized log list, facet
sidebar (click to add/exclude), detail side panel (pretty JSON attrs, "show
surrounding logs" = same stream ±N seconds, copy-as-filter on any attr),
live-tail toggle with pause-on-scroll, column chooser, saved views
(localStorage). A `trace_id` attr renders as a link (dead until M5).

## 7. Integrations
Per `docs/private/integrations.md` (M4 parts): compose bind-mount of app-node's
log dir; app-python opt-in JSON log formatter from the Python SDK
(`ozy.integrations.logging.JSONFormatter`) wired in api + worker when
ozymandias is enabled.

## 8. Test plan

- **L1** tailer with temp files + fake clock: append, partial line, long line,
  rotation-by-new-file, rotation-by-rename, copytruncate, truncation, file
  deleted while open, glob picks up new file, start_position semantics,
  registry persistence + resume, commit-only-after-ack; docker demux (frames
  splitting lines, interleaved stdout/stderr, TTY mode); multiline (traceback
  assembly, timeout flush, caps); each processor incl. every built-in grok
  pattern against real captured lines from all three apps (scrubbed, committed as testdata); Rails request grouping with interleaved concurrent requests;
  redaction rules (positive + negative cases — must not mangle normal text);
  logql parser + planner split; block seal thresholds; footer index; search
  ordering + cursor stability across pages while ingest continues; scan budget;
  retention; bloom tokenization + skip logic; SSE hub (filter, slow consumer
  drop, unsubscribe cleanup).
- **L2** log block encode/decode round-trip; `parse(print(q)) == q`; bloom: no
  false negatives ever; search result == brute-force filter over all appended
  logs (random logs × random queries).
- **L3** differential: v2 (bloom) results == v1 (grep) results for random
  corpora/queries.
- **L4** fuzz: docker stream demuxer, log chunk reader, logql parser, JSON/grok
  processors, `wire.DecodeLogs`.
- **L5** crash loop: SIGKILL agent mid-tail → restart → no lost lines
  (duplicates allowed, asserted ≤ one batch); SIGKILL ozyd mid-append →
  acknowledged logs present; torn chunk tail → valid prefix served; fake
  Docker daemon disconnects mid-stream → resume via `since` without loss.
- **L6** stress: 8 appenders + searches + tail subscribers under `-race`; hub
  leak check after 1000 subscribe/unsubscribe cycles.
- **L7** chunk-format goldens (v1 and v2); API goldens.
- **L8** in-process: write lines to a temp file → agent → intake → search finds
  them with correct status/service/attrs.
- **L9** smoke v4: `docker run --label … alpine echo`-style container emits a
  line → searchable; append to a mounted file → searchable; live tail receives it.
- **L10** Python `JSONFormatter`: output schema, exception → `error.stack`,
  extra fields → attrs, non-serializable values don't raise.
- **L11** Log Explorer: query-bar unit tests; Playwright search → facet click → detail panel → live tail.
- **L12** ingest lines/sec; bytes/line on disk (compression ratio); search
  latency for label-only vs free-text over 1 GiB raw, v1 vs v2.

## 9. Docs deliverables
`docs/formats/log-chunk.md` (v1 + v2); `docs/query-language.md` logql section;
`docs/api.md` logs + SSE; `docs/operations.md`: log source config, pipeline
reference, redaction rules, at-least-once semantics; DESIGN.md: log path,
delivery guarantees, stream model and why status is a label, the
Loki-vs-Elasticsearch trade with our measured numbers; diagram `log-path.mmd`;
ADR: index-light log store; the apps' docs; `docs/notes/M4.md`.

## 10. Acceptance criteria

- [ ] `service:app-node @tag:api @ms:>200` returns the slow requests; `service:app-python-worker status:error` shows judge errors with full multi-line tracebacks as single events.
- [ ] Midnight rotation of app-node's log file loses nothing (fake-clock integration test + one real overnight run noted).
- [ ] Restarting the agent neither loses nor re-sends more than one batch of lines.
- [ ] No JWT, reset token or `Authorization` value is findable by search after running app-python's signup/login/reset flows (explicit test).
- [ ] app-ruby: `service:app-ruby-api @controller:OrdersController @duration:>200` works on its plain-text Rails logs with no app change; placing an order with a phone number leaves **no phone number findable** in ozymandias (explicit scan test).
- [ ] Live tail shows a new line < 2s after it is written.
- [ ] Compression ratio and v1-vs-v2 search speedup reported in the notes.
- [ ] Tests and docs deliverables complete; `docs/notes/M4.md` has evidence.
