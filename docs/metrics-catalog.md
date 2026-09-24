# Metrics catalog

Every metric ozymandias emits about itself, and, from M1, every metric the
instrumented apps' integrations emit. `scripts/check-docs.sh` fails CI if a
metric registered in ozymandias's code is missing here.

Conventions (`docs/private/integrations.md` §4):
names are `lower.dotted`, units go in this table rather than in the name, and
tags come from bounded sets only.

## ozymandias self-metrics (`ozymandias.*`)

Readable at `GET /debug/vars` on both binaries, and queryable like any other
metric: the agent forwards its own every flush, and ozyd stores its own
every 10s (both tagged `host:<name>`). In the tables, a **counter** is
cumulative at `/debug/vars` and arrives in the store as a `count` of the
interval's increase.

| Metric | Type | Unit | Tags | Emitted by | Meaning |
|---|---|---|---|---|---|
| `ozy.build.info` | gauge | — | `component`, `version` | ozyd, agent | Always 1. The tags say which build is running |
| `ozy.process.uptime_seconds` | gauge | seconds | `component` | ozyd, agent | Time since the process started |
| `ozy.http.requests` | counter | requests | `component`, `route`, `status_class` | ozyd, agent | HTTP requests served. `route` is the matched pattern (e.g. `GET /healthz`), never the raw path. Unmatched requests are tagged `route:unmatched` |
| `ozy.agent.statsd.packets_received` | counter | datagrams | — | agent | UDP datagrams read from the socket |
| `ozy.agent.statsd.packets_dropped` | counter | datagrams | — | agent | Datagrams dropped because the parse queue was full. Kernel drops (socket buffer full) are invisible here — compare with what clients sent |
| `ozy.agent.statsd.messages_received` | counter | lines | — | agent | Metric lines parsed successfully |
| `ozy.agent.statsd.parse_errors` | counter | lines | — | agent | Malformed lines, each dropped alone |
| `ozy.agent.statsd.unsupported` | counter | lines | `kind` (`event`, `service_check`) | agent | extended StatsD events and service checks: recognized, counted, not stored |
| `ozy.agent.statsd.queue_length` | gauge | datagrams | — | agent | Datagrams waiting for a parse worker |
| `ozy.agent.aggregator.contexts` | gauge | series | — | agent | Live metric contexts (name + tag set) in the aggregator — the agent's cardinality |
| `ozy.agent.aggregator.points_flushed` | counter | points | — | agent | Points emitted by flushes |
| `ozy.agent.aggregator.flush_duration_ms` | gauge | ms | — | agent | How long the last flush took |
| `ozy.agent.aggregator.samples_dropped` | counter | samples | — | agent | Samples with a metric name that couldn't be normalized |
| `ozy.agent.aggregator.tags_dropped` | counter | tags | — | agent | Tags that couldn't be normalized, or beyond the 50-tag limit |
| `ozy.agent.aggregator.late_samples` | counter | samples | — | agent | Samples whose bucket had already been flushed, counted in the oldest open bucket instead |
| `ozy.agent.forwarder.payloads_sent` | counter | requests | — | agent | Payloads ozyd accepted |
| `ozy.agent.forwarder.payloads_rejected` | counter | requests | — | agent | Payloads refused with a non-retryable 4xx, dropped |
| `ozy.agent.forwarder.retries` | counter | attempts | — | agent | Failed attempts that were rescheduled with backoff |
| `ozy.agent.forwarder.dropped` | counter | series | — | agent | Series lost: memory cap (oldest first), a refused payload, or shutdown |
| `ozy.agent.forwarder.series_sent` | counter | series | — | agent | Series the intake reported as accepted |
| `ozy.agent.forwarder.series_rejected` | counter | series | — | agent | Series the intake reported as rejected (the reasons are logged) |
| `ozy.agent.forwarder.queue_bytes` | gauge | bytes | — | agent | Compressed payloads waiting to be sent or retried |
| `ozy.intake.series_accepted` | counter | series | — | ozyd | Series stored by `/v1/series` (and the self-report) |
| `ozy.intake.series_rejected` | counter | series | — | ozyd | Series refused, per reason in the response's `errors` |
| `ozy.intake.points_accepted` | counter | points | — | ozyd | Points stored |
| `ozy.store.series` | gauge | series | `store` | ozyd | Series in the metric store |
| `ozy.store.samples` | gauge | samples | `store` | ozyd | Samples in the metric store |
| `ozy.tsdb.head.series` | gauge | series | `store` | ozyd | Series in the in-memory head (not yet in a block) |
| `ozy.tsdb.head.chunks` | gauge | chunks | `store` | ozyd | Live chunks in the head; grows until a block is cut |
| `ozy.tsdb.ooo_rejected` | gauge | samples | `store` | ozyd | **Samples dropped** as out of order or older than the oldest writable block range. Non-zero means data is being lost — see `docs/operations.md` |
| `ozy.tsdb.series_limit_rejected` | gauge | series | `store` | ozyd | Series refused because a metric hit `max_series_per_metric` |
| `ozy.tsdb.blocks` | gauge | blocks | `store` | ozyd | Immutable blocks on disk; falls when compaction runs |
| `ozy.tsdb.disk_bytes` | gauge | bytes | `store` | ozyd | Total size of the store on disk, log included |
