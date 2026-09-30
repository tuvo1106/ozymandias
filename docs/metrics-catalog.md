# Metrics catalog

Every metric ozymandias emits about itself, and, from M1, every metric the
instrumented apps' integrations emit. `scripts/check-docs.sh` fails CI if a
metric registered in ozymandias's code is missing here.

## Naming conventions

Binding on this project's own metrics and on anything an integration emits:

- **`lower.dotted.names`.** The unit goes in this table, not in the name — the
  exception is a byte count, where `_bytes` / `.bytes` earns its place.
- **Durations are distributions** (`d`), in milliseconds when they come from an
  SDK. Anything in seconds says so in its row here.
- **Counts are counters** (`c`), named noun plus past participle:
  `submission.created`, not `create_submission`.
- **Tags come from bounded sets only.** Never a user id, an email address, a
  record id, a raw path, SQL, or an error message. High-cardinality identifiers
  belong on spans and log lines, which are built for them; a tag with unbounded
  values multiplies the series count without bound.
- **Scraped metrics keep their source names** under a namespace prefix, and
  Prometheus labels become tags under the same cardinality rules.
- **Every metric added is listed here.** `scripts/check-docs.sh` enforces it for
  this project's own instruments.

## ozymandias self-metrics (`ozy.*`)

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
| `ozy.agent.collector.runs` | counter | runs | `collector` | agent | Completed runs of a collector, successful or not |
| `ozy.agent.collector.errors` | counter | runs | `collector` | agent | Runs that returned an error (logged once per distinct error, and on recovery) |
| `ozy.agent.collector.timeouts` | counter | runs | `collector` | agent | Runs cancelled at their limit: `collectors.timeout`, or the collector's interval if that is shorter |
| `ozy.agent.collector.points` | counter | points | `collector` | agent | Points sent to the forwarder |
| `ozy.agent.collector.dropped` | counter | points | `collector` | agent | Points that could not be sent: a name the intake would refuse, a non-finite value, or emitted after the run ended |
| `ozy.agent.collector.tags_dropped` | counter | tags | `collector` | agent | Tags that could not be normalized; the point is kept without them |
| `ozy.agent.collector.duration_ms` | gauge | ms | `collector` | agent | How long the last run took |
| `ozy.agent.docker.events` | counter | events | — | agent | Container events read from the daemon's stream (start, oom, die) |
| `ozy.agent.docker.events_reconnects` | counter | reconnects | — | agent | Times the event stream ended, or failed to open, and was tried again (at most every 30s). Steady growth means the daemon keeps dropping the stream, or is not there: check the agent's log |
| `ozy.agent.docker.events_skipped` | counter | lines | — | agent | Event lines the agent could not decode (or over 1 MiB) and skipped; the stream carries on. Non-zero means a daemon speaking a format the agent does not know |
| `ozy.agent.autodiscovery.instances` | gauge | instances | — | agent | Checks running because a container asked for them with `ozy.check.*` labels |
| `ozy.agent.autodiscovery.errors` | counter | containers | — | agent | Containers whose `ozy.check.*` labels do not make a valid check (logged once per container) |
| `ozy.runtime.goroutines` | gauge | goroutines | `component` | agent | Live goroutines. One that climbs and never falls is a leak |
| `ozy.runtime.heap_bytes` | gauge | bytes | `component` | agent | Heap occupied by live and not-yet-swept objects |
| `ozy.runtime.gc_runs` | counter | cycles | `component` | agent | Garbage collections. A rising rate with a flat heap means allocation churn |
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
| `ozy.tsdb.wal_syncs` | gauge | syncs | `store` | ozyd | Write-ahead log flushes since startup (monotonic). Its *rate* is the check: ~1/`wal_sync_interval` while anything is being written, and a flat stretch means acknowledged samples are staying in the page cache longer than that |
| `ozy.intake.sketch_points_accepted` | count | points | | ozyd | Sketches accepted by `POST /v1/sketches`, one per series per bucket |
| `ozy.sketchstore.series` | gauge | series | | ozyd | Distribution series the sketch store holds |
| `ozy.sketchstore.disk_bytes` | gauge | bytes | | ozyd | Estimated size of the sketch store on disk |
| `ozy.sketchstore.points_appended` | count | points | | ozyd | Sketches written, one per series per bucket |
| `ozy.sketchstore.series_rejected` | count | series | | ozyd | Sketch series the store refused (a ref it cannot key, a timestamp outside the key range, or an id collision) |
| `ozy.sketchstore.id_collisions` | count | series | | ozyd | **Two series hashed to the same id.** One of them is being refused and its percentiles are missing. Expected to be zero forever — about one chance in 37 million at 100k series — so any value at all is worth a look; the log line names both series |

## Host metrics (`system.*`)

From the agent's host collector (`collectors.host`), every 15s, tagged
`host:<name>` like everything else the agent sends. They describe the kernel
the agent runs on: in compose, the Docker VM (Colima, Docker Desktop), not the
Mac; natively (`make dev`), the Mac. The exception is `system.net.*`: the
kernel keeps interface counters per network namespace, and the agent's
container has its own, so in compose they are the agent container's traffic,
not the VM's (`container.net.*` has every container's). A unit ending in **/s** is a
per-second rate the agent computed from two readings of a cumulative
counter, stored as a gauge (ADR-0026) so that it averages correctly over any
chart bucket; the first run after start has none, and a reading across a
counter reset is skipped rather than reported as a spike.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `system.cpu.user` | gauge | % | — | Share of all CPU time spent in user code, nice included, since the previous run. 0–100 across all cores together |
| `system.cpu.system` | gauge | % | — | In the kernel, interrupt handling included |
| `system.cpu.idle` | gauge | % | — | Idle |
| `system.cpu.iowait` | gauge | % | — | Idle with I/O outstanding (Linux; 0 on macOS) |
| `system.cpu.stolen` | gauge | % | — | Taken by the hypervisor for other guests (a VM only). The five states sum to 100 |
| `system.load.1` / `.5` / `.15` | gauge | processes | — | Load average over 1, 5 and 15 minutes |
| `system.mem.total` | gauge | bytes | — | Physical memory |
| `system.mem.used` | gauge | bytes | — | In use, as the kernel accounts it |
| `system.mem.free` | gauge | bytes | — | Unused. Near zero on a healthy Linux host, where spare memory is cache |
| `system.mem.usable` | gauge | bytes | — | Available to a new process without swapping (free plus reclaimable cache). The one to alert on |
| `system.mem.pct_usable` | gauge | fraction | — | `usable / total`, 0–1 |
| `system.swap.total` / `.used` / `.free` | gauge | bytes | — | Swap space |
| `system.swap.pct_free` | gauge | fraction | — | `free / total`, 0–1; absent without swap |
| `system.disk.total` / `.used` / `.free` | gauge | bytes | `device` | Space on each block device (`/dev/...`), once however many places it is mounted. On macOS, APFS volumes are one container and are reported once as `/dev/diskN`, with used = total − free. In a container: the Docker VM's data disk, seen through the container's bind mounts. Network mounts and folders shared from the Mac are not disks and are skipped |
| `system.disk.in_use` | gauge | fraction | `device` | `used / (used + free)`, 0–1: `df`'s Use%. Space reserved for root (5% on ext4) counts as neither, so `used + free` can be less than `total` |
| `system.io.r_s` / `.w_s` | gauge | operations/s | `device` | Read and write operations completed, per whole physical disk: partitions (whose I/O the disk already counts), loop, ram, device-mapper and RAID devices are left out, so a sum over devices counts each write once |
| `system.io.rkb_s` / `.wkb_s` | gauge | KiB/s | `device` | Read and written |
| `system.net.bytes_rcvd` / `.bytes_sent` | gauge | bytes/s | `interface` | Traffic per interface, in the agent's network namespace (in compose, its container's). Interfaces that never moved a packet, and those matching `collectors.host.exclude_interfaces`, are skipped |
| `system.net.packets_in.count` / `packets_out.count` | gauge | packets/s | `interface` | Packets |
| `system.net.packets_in.error` / `packets_out.error` | gauge | packets/s | `interface` | Packets with errors |
| `system.net.packets_in.drop` / `packets_out.drop` | gauge | packets/s | `interface` | Packets dropped |
| `system.uptime` | gauge | seconds | — | Time since the kernel booted |

## Container metrics (`container.*`, `docker.*`)

From the agent's Docker collector (`collectors.docker`), every 15s, for each
running container on the daemon the agent can reach. Every series carries
`container_name`, `container_id` (the short id; not on a name rewritten by
`container_name_rewrite`), `image_name`, `image_tag` (not for an image pinned
by digest or a bare id), and, for a compose container, `compose_project`,
`compose_service` and `service` (the `ozy.service` label, else
`<project>-<service>`). Tags with an empty value are left out. A **/s** unit
is a per-second rate stored as a gauge, as for `system.*`; a container's
first run has none.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `container.cpu.usage` | gauge | % | container | CPU used between this run's reading and the last, in % of one core (a busy 4-core container reads 400). Absent on a container's first run |
| `container.cpu.throttled` | gauge | periods/s | container | CFS periods in which the container hit its CPU quota. Non-zero means the limit, not the host, is slowing it |
| `container.memory.usage` | gauge | bytes | container | Memory charged to the container minus inactive file cache, which the kernel reclaims first (what `docker stats` shows) |
| `container.memory.limit` | gauge | bytes | container | The container's memory limit, or the host's memory if it has none |
| `container.memory.rss` | gauge | bytes | container | Anonymous memory: heap and stacks, which cannot be reclaimed without swap |
| `container.memory.cache` | gauge | bytes | container | Page cache charged to the container |
| `container.net.rx_bytes` / `.tx_bytes` | gauge | bytes/s | container | Received and sent, over all the container's interfaces. None on host networking |
| `container.io.read_bytes` / `.write_bytes` | gauge | bytes/s | container | Read from and written to block devices |
| `container.pids` | gauge | processes | container | Processes and threads in the container |
| `container.uptime` | gauge | seconds | container | Time since the container last started; back to 0 when it restarts in place |
| `docker.containers.running` | gauge | containers | `image_name` | Running containers per image |
| `container.exits` | count | exits | container, `exit_code`, `oom_killed` | Containers that stopped, from the event stream, so a container too short-lived for any poll is still counted. `exit_code:unknown` when the daemon did not say. A floor: exits while the daemon itself restarts are not replayed |
| `container.lifetime` | distribution | seconds | container | Start to exit, per exit; percentiles at query time. Missing when the start was neither seen nor could be inspected |

"container" in the Tags column means the container tags above. When a
rewrite folds several containers into one name, their amounts are summed
into one series, and `container.uptime` and `container.memory.limit` take the
largest. CPU % and the rates need two samples of a container, so a folded
series counts only the containers seen on the previous run too: with
sandboxes that live less than an interval, `container.cpu.usage` and the
`net`/`io` rates of the folded name undercount, and `container.memory.*`
and `container.exits` are the figures to trust.

## Check metrics

From configured or autodiscovered checks (`collectors.checks`, docs/operations.md). Every
metric of an instance carries that instance's `tags` and, when autodiscovered, the
container's tags. A configured instance with a name, or one of several unnamed ones, also
carries `instance:<name>` (or `instance:<position>`), so two instances of a check never
write the same series. For the same reason, containers that `container_name_rewrite` folds
into one name carry `replica:<n>` on their autodiscovered checks' metrics: the lowest
number free among that name's running replicas.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `openmetrics.up` | gauge | — | instance | 1 when the last scrape of the target succeeded, 0 when it failed (unreachable, non-200, unparseable, too large) |
| `openmetrics.scrape_duration` | gauge | seconds | instance | How long the last scrape took, failed or not |

What an `openmetrics` instance scrapes is named after the target's own metrics:
counters as per-second gauges, histograms as `<name>.bucket` / `.sum` / `.count`
counts (package doc of internal/agent/check/openmetrics).

## Redis check (`redis.*`)

From the `redis` check (docs/operations.md), per instance, from one INFO per
run. Every series carries the instance's tags. A **/s** unit is a
per-second rate of one of INFO's running totals, stored as a gauge
(ADR-0026); the first run, and the first after a server restart, have none.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `redis.can_connect` | gauge | — | — | 1 when the run connected (and authenticated) and INFO answered, else 0. Emitted every run |
| `redis.net.clients` | gauge | connections | — | Connected clients (`connected_clients`) |
| `redis.net.blocked` | gauge | connections | — | Clients blocked in BLPOP and friends (`blocked_clients`) |
| `redis.mem.used` | gauge | bytes | — | Memory Redis allocated (`used_memory`) |
| `redis.mem.rss` | gauge | bytes | — | Resident memory as the OS sees it (`used_memory_rss`) |
| `redis.mem.peak` | gauge | bytes | — | Highest `used_memory` since start (`used_memory_peak`) |
| `redis.mem.maxmemory` | gauge | bytes | — | The configured limit; 0 means none |
| `redis.mem.fragmentation_ratio` | gauge | ratio | — | RSS over used. Well above 1 is fragmentation; below 1 is swapping |
| `redis.net.commands` | gauge | commands/s | — | Commands processed (`total_commands_processed`): the server's throughput over the whole interval |
| `redis.stats.keyspace_hits` / `.keyspace_misses` | gauge | lookups/s | — | Key lookups that found / did not find the key. hits / (hits + misses) is the cache hit ratio |
| `redis.keys.evicted` | gauge | keys/s | — | Keys removed to stay under `maxmemory`. Non-zero means the cache is full |
| `redis.keys.expired` | gauge | keys/s | — | Keys removed because their TTL passed |
| `redis.net.rejected_connections` | gauge | connections/s | — | Connections refused at `maxclients` |
| `redis.keys` | gauge | keys | `db` | Keys per database (`db:db0`). A database with no keys has no series |
| `redis.expires` | gauge | keys | `db` | Keys with a TTL, per database |
| `redis.uptime` | gauge | seconds | — | Time since the server started |

## Postgres check (`postgresql.*`)

From the `postgres` check (docs/operations.md), every 15s per instance.
Every series carries `server:<host>` and `port:<port>`, plus the instance's
`tags`. A **/s** unit is a per-second rate computed from
`pg_stat_database`'s cumulative counters and stored as a gauge (ADR-0026);
the first run has none, and a run across `pg_stat_reset()` is skipped.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `postgresql.can_connect` | gauge | — | — | 1 when the run connected, 0 when it could not |
| `postgresql.connections` | gauge | connections | — | Client sessions in `pg_stat_activity`, the check's own included (all of them only with `pg_monitor`); not the server's background processes, which `max_connections` does not count |
| `postgresql.max_connections` | gauge | connections | — | The server's `max_connections` |
| `postgresql.percent_usage_connections` | gauge | % | — | connections / max_connections |
| `postgresql.commits` / `.rollbacks` | gauge | transactions/s | `db` | Transactions committed and rolled back |
| `postgresql.rows_returned` / `.rows_fetched` | gauge | rows/s | `db` | Rows read by sequential scans / fetched by index scans |
| `postgresql.rows_inserted` / `.rows_updated` / `.rows_deleted` | gauge | rows/s | `db` | Rows written |
| `postgresql.deadlocks` | gauge | deadlocks/s | `db` | Deadlocks detected |
| `postgresql.temp_bytes` | gauge | bytes/s | `db` | Written to temporary files by queries too big for `work_mem` |
| `postgresql.buffer_hit` | gauge | % | `db` | Share of block reads served from shared buffers over the last interval (not since startup). Absent in an interval with no block reads |
| `postgresql.database_size` | gauge | bytes | `db` | On disk. Absent without CONNECT privilege on the database |
| `postgresql.table_size` / `.index_size` | gauge | bytes | `schema`, `table` | For the tables listed in `relations`: the table (with TOAST) and all its indexes |

## HTTP check (`network.http.*`)

From each `http_check` instance, every run. Tagged `url:<scheme>://<host><path>`
(query string and credentials left out; lower-cased like every tag), plus
the instance's `tags`.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `network.http.can_connect` | gauge | 0/1 | `url` | A response arrived: 0 for refused, DNS failure, timeout before the headers or an untrusted certificate; 1 even if the body then fails |
| `network.http.up` | gauge | 0/1 | `url` | Connected, with an expected status, a body read in full and matching `content_match` if set. The one to alert on |
| `network.http.status_code` | gauge | status | `url` | The response's status (after redirects, unless `follow_redirects: false`) |
| `network.http.response_time` | gauge | seconds | `url` | From sending the request to reading the body (up to 64 KiB) on a new connection: DNS, connect, TLS and transfer |
| `network.http.ssl.days_left` | gauge | days | `url` | Until the server certificate's NotAfter (https only; fractional) |

## Process check (`system.processes.*`)

From each `process` instance, every run: the matching processes together,
tagged `process_name:<label>`. With no match only `number` (0) is sent.

| Metric | Type | Unit | Tags | Meaning |
|---|---|---|---|---|
| `system.processes.number` | gauge | processes | `process_name` | Matching processes |
| `system.processes.cpu.pct` | gauge | % | `process_name` | CPU used since the previous run, in % of one core, summed per process (each differenced against its own previous reading, so processes coming and going do not distort it). None on a process's first run |
| `system.processes.mem.rss` | gauge | bytes | `process_name` | Resident memory, summed |
| `system.processes.threads` | gauge | threads | `process_name` | Threads, summed |
| `system.processes.open_file_descriptors` | gauge | descriptors | `process_name` | Open file descriptors, summed; absent when any match's count cannot be read (another user's process, without privileges) |
