# M7 — Pipeline hardening

**Goal:** make the pipeline behave like a production one under failure:
decouple intake from storage with a durable queue, propagate backpressure
end to end, buffer on disk at the agent, add API keys and limits, and prove it
all with chaos drills.

**Concepts learned:** why ingestion and storage are decoupled (Kafka's role in
a production pipeline); durable logs and consumer offsets; at-least-once + idempotent
consumers; backpressure vs load shedding; bounded everything; failure drills
as tests.

## 1. Durable queue — `internal/queue` (a mini-Kafka, single node)

- Topics: `series`, `sketches`, `logs`, `traces`. Each = a directory of
  segments (reuse the `wal` segment/record/CRC code): record =
  `offset u64 | ts | payload` where payload is the **validated, decoded-and-re-encoded**
  request body (compact binary or JSON — choose one, document it).
- `Produce(topic, payload) (offset, error)` returns after group-commit fsync
  (default every 5 ms or 1 MiB — measure the latency/throughput trade and
  report).
- Consumers: one per topic per store. `Poll(max) → []Record`, process, then
  `Commit(offset)` (consumer offsets in a small fsynced file, written
  atomically). Crash between process and commit → redelivery, so **store
  appends must be idempotent**: TSDB already no-ops duplicate `(t,v)` and
  rejects older; logs dedupe by `(streamID, ts, hash(message))` within the
  head block; spans overwrite by key; sketches overwrite by key.
- Retention: delete segments fully below `min(committed offsets)`; hard cap
  `queue.max_bytes` (default 2 GiB) per topic.
- Intake now: authenticate → decode/validate → `Produce` → `202`. It no longer
  touches stores. Consumer lag exposed as `ozy.queue.lag_records` /
  `lag_seconds` per topic.
- Config `pipeline.mode: direct | queued` — `direct` (pre-M7 path) stays for
  tests and comparison. (Optional exercise, not required: a NATS JetStream
  implementation behind the same interface.)

## 2. Backpressure and load shedding

- Queue above 80% of `max_bytes` → intake answers `429` + `Retry-After: 5`
  for `logs` and `traces` first, `series`/`sketches` only at 95% (metrics are
  the cheapest and most valuable — shed the expensive signals first).
- Live-tail hub, query engine and monitors are isolated from ingest pressure:
  separate goroutine pools; query concurrency limit (8) with queueing + 503
  when saturated.
- Per-request limits from wire-protocol §0 enforced; per-key rate limits (§4).

## 3. Agent disk buffer

Forwarder retry queue spills to disk: `buffer_path`, `max_disk_bytes`
(default 512 MiB), per-endpoint FIFO files with CRC'd records; memory queue
first, overflow to disk, replay oldest-first on recovery; drop-oldest at the
cap with counters. Survives agent restart. Priority when draining after an
outage: series → sketches → logs → traces. Log-tailer offsets still commit
only after a 2xx (from M4), so a full buffer *pauses tailing* rather than
dropping log lines (files are their own buffer) — statsd and traces are shed.

## 4. API keys, tenancy-lite, limits

- `api_keys(id, name, key_sha256, created_at, last_used_at, revoked_at, limits_json)`;
  key shown once at creation; constant-time compare on the hash.
  `auth.enabled: true` → `/v1/*` requires `X-Ozy-Key`; agent config
  `api_key` / env `OZY_API_KEY`. `/api/v1/*` + UI protected by a single
  admin password → session cookie (HttpOnly, SameSite=Lax); good enough for
  local/VPS, documented as not multi-user.
- Limits per key: requests/s, bytes/s (token buckets), max series per metric,
  max total active series, max log bytes/day, max spans/s. Exceeding →
  429 or item-level reject with reason; all counted under
  `ozy.intake.rejected{reason:…}`.
- Cardinality tooling: Metric Summary gains "top growing metrics" and
  tag-key cardinality; an optional per-metric **tag allowlist/denylist**
  (`metric_tag_config`) applied at the consumer to drop high-cardinality tags.

## 5. Operability

- `/readyz` (stores open, queue writable, WAL replay done) vs `/healthz`.
- Startup order + WAL replay progress logged; graceful shutdown drains
  consumers to a commit point.
- `ozyd backup <dir>` — consistent snapshot: pause compaction, hardlink
  blocks + chunks, Pebble checkpoint, SQLite `VACUUM INTO`, queue offsets.
  `ozyd restore`.
- "ozymandias self" dashboard completed: every stage's throughput, lag, drops,
  latencies, disk usage by component.

## 6. Chaos drills — `scripts/chaos/*.sh` (each asserts its expected outcome)

| Drill | Expected |
|---|---|
| `kill -9` ozyd during sustained ingest, restart | zero acknowledged-data loss; agent buffers and drains; no duplicates visible in queries |
| ozyd down 15 min | agent memory bounded, disk buffer grows then drains in priority order; app latency unaffected |
| agent down | apps unaffected (UDP drops silently; tracer drops + counts); log lines caught up from file offsets on restart |
| fill the data disk (small tmpfs volume) | intake 429/503s, no corruption, no panic; recovers after space is freed |
| UDP flood 500k pkt/s for 60s | agent stays up, drops counted, other pipelines (logs, traces) keep flowing |
| slow disk (fsync delay injected via fault FS / `dm-delay` optional) | backpressure engages; acks never precede durability |
| 1M-series cardinality bomb from one key | series limit holds; other metrics unaffected; memory bounded |
| Pebble/TSDB block corruption (bit-flip a file) | detected, block quarantined, rest served, loud self-metric + log |
| clock jump ±1h on the agent host | future/ancient points rejected and counted; no head corruption |
| all three apps' test suites with ozymandias in each failure state | green (app-ruby has no runtime dependency on ozymandias at all until M8 — assert its `/metrics` latency is unaffected by a hung scraper) |

## 7. Test plan

- **L1** queue: produce/poll/commit, segment roll, retention by committed
  offset and by size, offset file atomicity; intake in queued mode; shed
  ordering by topic; token buckets (fake clock); key lifecycle (create, use,
  revoke, last_used), constant-time compare; limits → correct reject reasons;
  tag allow/deny; disk buffer FIFO, spill, replay order, cap, corruption of a
  buffer record skipped; readiness transitions; backup/restore round-trip.
- **L2** queue: consumed sequence == produced sequence for any interleaving of
  produce / crash-truncate / restart; idempotent consumers: applying any
  record sequence with arbitrary duplications yields the same store state as
  applying it once (per store — this is the key property of the milestone).
- **L3** `direct` vs `queued` mode produce identical query results for a
  random workload.
- **L4** fuzz: queue segment reader, disk-buffer reader, key header parsing.
- **L5** crash loop in queued mode (SIGKILL at random points incl. between
  process and commit); fault FS on queue fsync → no 202 without durability;
  ENOSPC behaviour.
- **L6** produce/consume/retention concurrently under `-race`; leak checks.
- **L7** queue segment + disk buffer format goldens.
- **L9** smoke v7 runs with `auth.enabled` + queued mode; wrong key → 401;
  every chaos drill runnable in CI's nightly job (a subset — kill/restart,
  agent down, disk full on tmpfs — on every push).
- **L12** end-to-end latency (statsd packet → queryable) p50/p99 in direct vs
  queued mode; max sustained ingest per signal; fsync group-commit sweep.

## 8. Docs deliverables
`docs/formats/queue-segment.md`, `docs/formats/agent-buffer.md`; DESIGN.md:
the decoupled pipeline, delivery semantics per signal end-to-end (a table:
where at-most-once, where at-least-once, where data can be lost and why that
is acceptable), idempotency argument per store, backpressure map, shedding
priority; `docs/operations.md`: auth, keys, limits, backup/restore, capacity
planning from measured numbers, **runbook** per chaos drill (symptom → check →
fix); `docs/diagrams/pipeline-queued.mmd`; ADRs: own queue vs Kafka/NATS,
shed order, single-admin auth; `docs/notes/M7.md` with each drill's evidence.

## 9. Acceptance criteria

- [ ] Every drill in §6 passes and is scripted.
- [ ] Queued mode: `kill -9` loop (50×) shows zero acknowledged loss and no visible duplicates.
- [ ] With ozymandias in any failure state, app-node and app-python p99 request latency is unchanged (< 1 ms added; measured with and without the SDK enabled).
- [ ] Auth on: unauthenticated intake and UI are rejected; agent + apps work with keys.
- [ ] End-to-end latency and max-throughput numbers recorded for both modes.
- [ ] Tests and docs deliverables complete; `docs/notes/M7.md` has evidence.
