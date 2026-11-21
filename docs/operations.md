# Operations

Running ozymandias: how to start it, configure it, check its health, and
troubleshoot it. The design behind each piece is in [../DESIGN.md](../DESIGN.md).

## Running it

| How | Command | Use when |
|---|---|---|
| Compose | `make up` → `make smoke` → `make down` | The production-shaped stack: one image, both services, a named data volume |
| Native | `make dev` | Day-to-day work: both binaries from `./bin`, plus the Vite dev server with hot reload on :9401. Ctrl-C stops all three |
| By hand | `./bin/ozyd -config deploy/ozyd.yaml` | Debugging one binary |

`make up` builds the image, creates the external `ozymandias` Docker network if
it's missing, starts both services, and waits until both are healthy. It
stamps the image with `git describe`, and passes the machine's name as
`OZY_HOSTNAME` ([scripts/hostname.sh](../scripts/hostname.sh): an exported
value, else on macOS the LocalHostName, which unlike `hostname -s` does not
change with the network). It becomes the `host` tag on the agent's data and
on ozyd's own metrics, so both processes agree and neither tag changes when a
container is recreated. `make dev` does the same. `make down-v` also deletes the data
volume.

| Port | Service | Purpose |
|---|---|---|
| 9400/tcp | ozyd | UI, `/api/v1/*` (M1), intake `/v1/*` (M1), `/healthz`, `/debug/vars` |
| 8126/tcp | agent | Trace intake from SDKs (M5), `/healthz`, `/debug/vars` |
| 8125/udp | agent | extended StatsD metrics from SDKs (M1) |
| 9401/tcp | Vite | UI dev server (`make dev` only) |

## Configuration

Both binaries read one YAML file (`-config`), then environment overrides. The
agent also reads conf.d fragments in between.
[deploy/ozyd.yaml](../deploy/ozyd.yaml) and
[deploy/agent.yaml](../deploy/agent.yaml) are the complete, commented
reference, and every key is listed there with its default.

- Environment overrides: `OZY_<PATH>` / `OZY_AGENT_<PATH>`, the YAML
  path upper-cased and joined with `_`, e.g. `OZY_AGENT_INTAKE_URL`. Lists
  are comma-separated.
- Per-app agent settings go in `deploy/agent.d/<app>.yaml`. Compose mounts that
  directory read-only, so editing a fragment needs only an agent restart, not
  a rebuild.
- A typo in a file fails startup with the file and line. A typo in an
  environment variable is logged as a warning at startup, so read the first
  lines of the log after changing the environment.

### Agent: the metric pipeline (M1)

The defaults suit one host running a handful of apps. Each knob below is in
[deploy/agent.yaml](../deploy/agent.yaml); raise them only in response to a
metric, and the metric to watch is named in the last column.

| Key | Default | What it controls | Raise it when |
|---|---|---|---|
| `statsd.enabled` | `true` | bind the UDP listener at all | — |
| `statsd.addr` | `:8125` | listen address | — |
| `statsd.readers` | `2` | goroutines calling `ReadFromUDP` | the kernel drops datagrams before the agent sees them (a gap between what apps sent and `packets_received`) |
| `statsd.workers` | `2` | goroutines parsing and aggregating | `queue_length` sits high while readers keep up |
| `statsd.queue_size` | `1024` | datagrams buffered between readers and workers | `packets_dropped` climbs in short bursts |
| `statsd.read_buffer` | `4194304` | `SO_RCVBUF`, the kernel's own queue | drops happen faster than the agent can react (the first thing to raise) |
| `aggregator.flush_interval` | `10s` | bucket width and flush period | rarely — it is the resolution of every chart |
| `aggregator.context_expiry` | `300s` | how long an idle context is kept (and a counter zero-filled) | charts should keep showing 0 for longer after an app stops |
| `aggregator.histogram_max_samples` | `10000` | reservoir size per histogram context | percentiles look noisy (costs memory per context) |
| `forwarder.timeout` | `10s` | per-request timeout to the intake | the intake is legitimately slow |
| `forwarder.max_queue_bytes` | `67108864` | retry buffer; oldest payloads drop first | outages regularly outlast the buffer (watch `forwarder.dropped`) |
| `forwarder.shutdown_timeout` | `5s` | budget for the last flush on SIGTERM | the final flush is being cut short |

Two ordering rules matter operationally:

- **Stop the agent before ozyd.** The agent's final flush needs the
  intake still listening. `make dev` and compose's `depends_on` both encode
  this; a manual `kill` of both at once loses the last bucket.
- **A restart of ozyd is safe.** The agent retries with backoff and keeps
  up to 64 MiB of payloads, so a two-minute restart loses nothing (measured in
  [benchmarks.md](benchmarks.md)).

### Agent: collectors (M3)

Collectors read a source on a timer and send what they find, every
`collectors.interval` (15s). Each is independent: one that fails or hangs
does not delay another. Per collector, `ozy.agent.collector.runs`,
`.errors`, `.timeouts` and `.duration_ms` (tag `collector`) say how it is
doing; a failure is logged once when it starts and once when it clears, not
every run.

| Collector | Config | Reports | Notes |
|---|---|---|---|
| `host` | `collectors.host` | `system.*` | In compose, the Docker VM's kernel, not the Mac's: its CPUs, memory and disks — but network counters are per namespace, so `system.net.*` is the agent container's own traffic. `make dev` runs the agent natively and reports the Mac. Rates need two readings, so the first 15s after start have gauges only |
| `docker` | `collectors.docker` | `container.*`, `docker.containers.running`, and from the event stream `container.exits`, `container.lifetime` | Every container on the daemon behind `collectors.docker.socket`. In compose that is the Docker VM's daemon, reached through the mounted socket and the socket's group, which `make up` looks up (ADR-0028); `docker compose up` by hand gets gid 0 and, under Colima, permission denied — logged once, the rest of the agent unaffected. `make dev` uses the docker context's socket. Name many short-lived containers as one with `container_name_rewrite` (the app-python fragment does, for its judge sandboxes); names no rule groups are capped at `max_container_names` (200), the rest reported as `other` (ADR-0036) |

**Socket access is root on the Docker VM.** Anything that can call the
Docker API can start a privileged container. The agent only reads (list,
stats, inspect, events), but for a deployment where the host matters, put a
GET-only proxy in front that serves its own unix socket (the agent's client
speaks only unix sockets, not TCP) and point `collectors.docker.socket` at
that, or disable the collector.

#### Checks and autodiscovery

Checks are configured under `collectors.checks.<check>.instances`
(deploy/agent.yaml has the shape), or by labels on a container:

```yaml
services:
  cache:
    image: redis:7
    labels:
      ozy.check.redis.host: "%%host%%"
      ozy.check.redis.port: "%%port%%"
```

The agent lists containers every 10s; the check starts as
`redis:<container name>`, tagged like the container's metrics, and stops
with it. (One exception: if the image's tag was moved to a new build while
the container kept running, and the agent restarted since, the check's
`image_name`/`image_tag` come from the image id Docker then lists, while
the container's own metrics keep the name it was started from.) The name is the one after `container_name_rewrite`. Containers that rewrite
folds into one name are each checked, and their metrics carry a
`replica:<n>` tag (the lowest number free among that name's running
replicas) so that they do not overwrite each other; they share one set of
`ozy.agent.collector.*` self-metrics. Sum or average across `replica` at
query time, as the metric calls for. A discovered instance may not take the
name of a configured one (a container `cache` with redis labels beside a
configured `redis` instance named `cache`): it is refused and logged, and
an `ozy.check.<check>.name` label gives it another.

`%%host%%` is the container's IP address on a network, so **the agent must
share a Docker network with the container** — in compose, list the app's
network on the agent service (or the app joins `ozymandias`).
`collectors.docker.autodiscovery_network` names that network (the shipped
config says `ozymandias`); unset, a container must be on exactly one
network, and one on several is refused, since its other addresses may have
no route from the agent.

A label `ozy.check.<check>.<setting>` sets exactly that check's setting, as
the check's table below lists it (ADR-0032); there are no label-only
shortcuts. An exporter is scraped with
`ozy.check.openmetrics.url: "http://%%host%%:%%port%%/metrics"`.

A label value reaches a setting as plain text that the check decodes:
`"6379"` is a number where the setting wants one, and a text setting gets
the value exactly as written (a password `0123` or `pa ss #1` stays as it
is). A list is written in brackets, `"[200, 301]"`, and a mapping in
braces, `"{Authorization: Bearer x}"`; either is read so only where the
setting is a list or a mapping. Labels that do not make
a valid check are logged once and counted in
`ozy.agent.autodiscovery.errors`. Settings are resolved again every sync: a
container restarted with a new address gets its check rebuilt, and one
that failed for want of an address is tried again once it has one.

#### Several instances of one check

Configured instances of one check are told apart by the tag
`instance:<name>` on every metric (`instance:0`, `instance:1`, … for
unnamed ones), since most checks' metrics say nothing about their target:
two redis instances would otherwise write the same `redis.*` series and
overwrite each other. A single unnamed instance gets no such tag, an
instance whose `tags` already has an `instance:` tag keeps its own, and two
names that differ only in case are refused. Discovered instances carry
their container's tags instead.

#### Check: openmetrics

Scrapes a Prometheus or OpenMetrics `/metrics` page every run. Counters become
per-second rates (sent as gauges, ADR-0026), gauges stay gauges, histograms
become per-bucket counts `<name>.bucket` tagged `upper_bound` plus `.sum` and
`.count`, and summaries a `quantile`-tagged gauge plus `.sum` and `.count`.
Every label becomes a tag; one whose key the instance or the agent
already tags with another value (`service`, `container_name`, `instance`,
an agent-wide `env`…) becomes
`exported_<key>`, as Prometheus renames it, so a series never has two
values for one key. A `host` label is always `exported_host`: on a scraped
page it is a Host header or virtual host, not the machine, and as `host` it
would move the series off the agent's. `openmetrics.up` and `openmetrics.scrape_duration`
say whether the scrape worked.

```yaml
collectors:
  checks:
    openmetrics:
      instances:
        - name: caddy
          url: http://caddy:2019/metrics
          namespace: caddy
          metrics: ["^caddy_http_"]
```

| Setting | Default | Meaning |
|---|---|---|
| `url` | required | The page, http or https. Autodiscovery fills `%%host%%` and `%%port%%` |
| `namespace` | none | Prefixed to every scraped metric's name with a dot |
| `metrics` | all | Regexes; only metrics whose name matches one are kept. The name is the sample name for counters and gauges, the family name for histograms and summaries |
| `exclude` | none | Regexes; matching metrics are dropped |
| `rename` | none | Map of name to new name, applied before `namespace` |
| `exclude_labels` | none | Labels not turned into tags |
| `timeout` | `10s` | One scrape |
| `max_body` | 10 MiB | Bytes; a larger page is an error (up 0) |
| `max_series` | 2000 | Metrics one scrape may emit, and series (counters, histograms) the check remembers between scrapes; the rest are dropped and the run reports an error |
| `histogram_buckets_as_distributions` | `false` | Send each histogram as one distribution (a DDSketch of the interval's buckets) named after the family, instead of `.bucket` counts, so `p90:` works on it. Lossy: a percentile is as precise as the bucket widths |

The first scrape has gauges only: a rate or a count needs two. A target that
restarts is detected (a counter went down): its rates skip one scrape, and
its histogram counts restart from what it counted since.

#### Check: redis

Connects once per run, sends INFO (plain, so Redis before 7 answers too),
and reports `redis.*`. Settings, besides the common `name`, `interval` and
`tags`:

| Setting | Default | Meaning |
|---|---|---|
| `host` | required | Name or address, or a unix socket path starting with `/` |
| `port` | 6379 | A number or a numeric string (label values are strings) |
| `username`, `password` | none | AUTH after connecting; `username` needs Redis 6 ACLs. The password appears in no log, error or tag |
| `db` | 0 | SELECTed after AUTH. INFO is server-wide either way |
| `timeout` | 5s | Bounds the connection and INFO |

```yaml
collectors:
  checks:
    redis:
      instances:
        - name: cache
          host: redis
```

With autodiscovery, a container labelled `ozy.check.redis.host=%%host%%`
gets an instance named `redis:<container name>` (after
`container_name_rewrite`).

#### Check: postgres

Reports a PostgreSQL server as `postgresql.*` (docs/metrics-catalog.md),
connecting once per run with pgx (ADR-0031) and disconnecting afterwards.

```yaml
collectors:
  checks:
    postgres:
      instances:
        - name: main          # collector tag postgres:main
          host: db
          port: 5432          # default; a numeric string also works
          user: ozy_monitor
          password: ...       # never logged; prefer a login with pg_monitor
          dbname: postgres    # default: where it connects, not what it reports
          sslmode: disable    # default; disable|allow|prefer|require|verify-ca|verify-full
          timeout: 5s         # default; bounds the whole run
          relations: [orders] # optional: tables whose sizes to report, at most 100
```

Every database the server has is reported (tag `db`), except templates and
those that allow no connections, whichever `dbname` the check connects to. A
login with the `pg_monitor` role sees every session in `pg_stat_activity`;
without CONNECT privilege on a database, its size is left out.
`postgresql.can_connect` is 0 when the server cannot be reached, and the
failure is logged once.

The unit tests need no server. To check the SQL against a real one:

```console
$ docker run -d --name ozy-pgtest -p 55432:5432 -e POSTGRES_PASSWORD=pw postgres:16-alpine
$ OZY_TEST_POSTGRES_HOST=127.0.0.1 OZY_TEST_POSTGRES_PORT=55432 OZY_TEST_POSTGRES_USER=postgres \
    OZY_TEST_POSTGRES_PASSWORD=pw go test -run Integration -v ./internal/agent/check/postgres/
$ docker rm -f ozy-pgtest
```

#### Check: http_check

Requests one URL per run and reports `network.http.*`. Settings, besides
the common `name`, `interval` and `tags`:

| Setting | Default | Meaning |
|---|---|---|
| `url` | required | An absolute `http://` or `https://` URL |
| `method` | `GET` | |
| `timeout` | `5s` | The whole request, body included |
| `expected_status` | any 2xx or 3xx | A list of statuses that count as up |
| `content_match` | — | A regular expression the first 64 KiB of the body must match |
| `tls_skip_verify` | `false` | Accept any certificate (`ssl.days_left` is still reported) |
| `headers` | — | A map of request headers; `Host` sets the virtual host |
| `follow_redirects` | `true` | `false` reports the redirect itself (e.g. http → https) |

```yaml
collectors:
  checks:
    http_check:
      instances:
        - name: shop
          url: https://shop.example/healthz
          content_match: '"status":"ok"'
```

A run that is not up logs why once (and again when it recovers) and emits
`network.http.up 0`. The `url` tag is `scheme://host/path` only: the query
string and any `user:password@` are left out, since that is where tokens
live, and a URL that cannot be a tag (a comma in it) is sent untagged.
Every connection is new (no keep-alive), so the time includes connecting.

#### Check: process

Finds processes and reports their count and summed resource use as
`system.processes.*`, tagged `process_name:<label>`. It reads the agent's
process table, so it is configured only: from a container's labels it is
refused, since it could not count that container's processes. Exactly one
of:

| Setting | Meaning |
|---|---|
| `process_name` | The process name, exactly (`exact_match: false`: anywhere in the command line). Not `name`, which is the instance's own name, as for every check |
| `pattern` | A regular expression over the command line; needs `label` |
| `pid_file` | A file holding a pid; needs `label`. A missing file means not running (count 0) |

`label` sets the tag's value (default `process_name`).

```yaml
collectors:
  checks:
    process:
      instances:
        - process_name: postgres
        - pattern: 'uvicorn .*app:api'
          label: api
```

**The agent sees only its own pid namespace.** In compose that is the
agent's container, so the check finds nothing there unless the agent
service runs with `pid: host` (it then sees every process on the Docker
VM). Natively (`make dev`) it sees the Mac's processes, but counting
another user's file descriptors needs privileges, so
`open_file_descriptors` may be absent.

## Health and self-metrics

```console
$ curl -s localhost:9400/healthz
{"component":"ozyd","status":"ok","uptime_seconds":42,"version":"v0.1.0"}
$ curl -s localhost:8126/debug/vars       # the agent's ozy.* metrics
$ docker compose -f deploy/docker-compose.yml ps   # includes each container's health
```

Inside the distroless images there is no shell or curl. Each binary checks
itself with `ozyd healthcheck` / `agent healthcheck`, which read the same
config and probe `/healthz` over loopback. Compose's healthchecks use them.

### Readiness

`/healthz` says the process is alive; `/readyz` says whether to send it traffic. They are
different questions and an orchestrator that is given the wrong one makes things worse.

```console
$ curl -s -o /dev/null -w '%{http_code}\n' localhost:9400/readyz
200
$ curl -s localhost:9400/readyz        # when it is not
{"checks":{"consumers":"ok","draining":"ok","logstore":"wal: log is no longer writable: …","meta":"ok","tsdb":"ok"},"status":"not_ready",…}
```

`/readyz` is 503 while any of these fails: `meta` (the SQLite database does not answer), `tsdb`
and `logstore` (the store stopped itself after a failed fsync), `queue:<topic>` (a topic
stopped the same way, queued mode), `consumers` (queued mode: fewer consumers running than
topics), `draining` (shutdown has begun). It is public, like `/healthz`. With `auth.enabled`, only a caller on this host sees the reason for a failure (it can name paths under `data_dir`); anyone else gets `"failed"`, and the reason is logged once per change.

**A stopped store.** When an fsync of a store's write-ahead log fails, the kernel may already
have dropped the pages, and a later fsync can report success without writing them, so ozyd does
not retry: the log stops, every write to that store fails (a 503 at intake, a retried record in
queued mode), `ozy.store.broken{store}` goes to 1 and `/readyz` says which. `/healthz` stays
green: the process is fine and restarting it is the repair (replay makes the log trustworthy
again). Point the orchestrator's *readiness* probe at `/readyz`, alert on `ozy.store.broken`,
and restart on it.

**Shutdown order.** On SIGTERM `/readyz` goes 503 at once. With `http.drain_delay` set, ozyd keeps
serving for that long so a load balancer polling `/readyz` takes the instance out before the listener
closes; then in-flight requests finish (`http.shutdown_timeout`), the queue consumers stop at a commit
point (a batch is applied, its stores synced, and its offset committed, or none of them: what was applied and not committed is
delivered again on the next start), and the stores close.

## Backup and restore

With `backup.root` set (an absolute path outside `data_dir`), a running ozyd can back itself up:

```console
$ ozyd backup -config deploy/ozyd.yaml -name nightly
backup written to /var/backups/ozymandias/nightly (143 files, 88103251 bytes, written by v0.1.0 at 2026-11-14T09:30:00Z)
$ ozyd restore -verify /var/backups/ozymandias/nightly      # check it against its manifest
$ ozyd restore -config deploy/ozyd.yaml /var/backups/ozymandias/nightly   # offline, into an EMPTY data_dir
```

- With auth off, `/api/v1/admin/backup` answers only requests from this host (which is what `ozyd backup` is); with auth on it needs the admin login from anywhere. A failed backup removes what it wrote, and shutdown waits for a running one.
- It is online: ozyd keeps serving. Each store is snapshotted consistently under its own locks
  (blocks hard-linked, logs cut to a length, SQLite by `VACUUM INTO`, Pebble by checkpoint). Ingest into the
  log store waits for the copy of the log store (its write lock is held); the others take brief locks.
- The queue is cut first and the stores after, so a restored ozyd redelivers what its stores and its queue
  both hold, and the stores absorb it. One gap, unchanged from queued mode generally: a log sealed
  into a chunk between the two cuts can be stored twice after a restore.
- A backup is a directory plus `MANIFEST.json` (docs/formats/backup.md). `restore` verifies every file's
  size and SHA-256 and that nothing extra is there, *before* it writes, and refuses a `data_dir` that is
  not empty. It copies; it never links. Then start ozyd on it: it recovers as it does after a crash.
- **A backup of hard links is not off-disk.** Same filesystem, it shares bytes with the live data: it
  protects against ozyd's bugs and operator mistakes, not the loss of the disk. Put `backup.root` on another
  volume (links then fall back to copies) or copy the directory away.
- Monitor `ozy.backup.last_success_timestamp` (unix seconds, 0 means none since start) and `ozy.backup.runs{result}`.
- Not supported: `metric_store: naive` (the backup says so and fails), and a restore onto a running ozyd.
  Not yet measured: backup time and size against data size, and a backup taken during a crash.

## Failure runbook and the chaos drills

Each drill in `scripts/chaos/` breaks one thing on purpose and asserts what must still be true
(`make chaos` runs them all in about three minutes; `make chaos-long` at the acceptance settings,
about 40). They run ozyd and the agent from `./bin` in a temp directory, so they never touch a real
data dir. What an operator sees, and does, for each:

| Symptom | Check | What is true, and the fix |
|---|---|---|
| ozyd was killed (`kill -9`, OOM, power) | `/readyz` 200 after restart; `ozy.consumer.*` counters moving | Everything answered `202` is in the queue or the stores. Restart; replay and the consumers do the rest. A request that got no answer may or may not have landed: senders retry, and the stores absorb a repeat (`kill9.sh`). |
| ozyd is down, agents are up | agent: `ozy.agent.forwarder.spool_bytes`, `…spool_payloads` | Agent memory stays bounded; payloads spill to `forwarder.buffer_path` and drain in order after ozyd returns, about 20 s after, once the retry backoff expires. Set `buffer_path`: without it only the memory queue (64 MiB) holds data (`ozyd-down.sh`). |
| the agent is down | app: nothing; `agent` `/healthz` | The app's statsd sends do not fail or block. Log files are caught up from the saved offsets on restart, with none repeated; metrics sent while it was down are lost (UDP) (`agent-down.sh`). |
| UDP flood at the agent | `ozy.agent.statsd.packets_dropped`, `loadgen -scenario statsd-flood` | The agent stays up; loss is counted (queue drops) or invisible (kernel drops: raise `statsd.read_buffer` and the OS limit); traces and logs, which do not use the UDP socket, keep flowing (`udp-flood.sh`). |
| `ozy.tsdb.ooo_rejected` or `ozy.intake.series_rejected` rising | the sender's clock (`date`, NTP) | A point more than 10 minutes ahead is refused at intake; one older than the series' newest is refused by the store. Neither corrupts anything, including across a restart. Fix the clock (`clock-jump.sh`). |
| `ozy.queue.lag_*` growing with `ozy.consumer.sync_errors` rising or `consumer: store failed` / `store sync failed` in `ozyd.log` (queued mode) | `/readyz`: it names the store whose log stopped (`tsdb`, `logstore`). The sync counter can stay 0 if the TSDB's own periodic sync hit the failure first | A store's fsync failed. The WALs are fail-stop: the store refuses writes until restart, so the topic backs up behind it and intake answers 429 when it fills. Fix the disk, restart: the uncommitted batch is redelivered from the queue and stored again (ADR-0063). |
| `ozy.tsdb.series_limit_rejected` rising | which tag carries an unbounded value | `storage.max_series_per_metric` holds, other metrics are unaffected, memory is bounded. In queued mode the sender is not told (it gets `202`): look at `ozy.consumer.series_rejected`. Drop the tag in the app (`cardinality-bomb.sh`). |
| `ozy.tsdb.blocks_quarantined` > 0, or `block quarantined` (ERROR) in `ozyd.log` | the log line: the block, where it moved, why, and the time range it covered (`min_time`..`max_time`) | A block of the metric store was damaged (a bad index, a missing or short file, a lost `meta.json`) and ozyd started without it (ADR-0064). Nothing was deleted: it is `blocks/<ulid>.corrupt-<unix>`. **Queries over that time range return less than was stored, and the API does not say so.** Fix the cause (the disk) first. To restore it: repair or copy back the damaged file, rename the directory to `<ulid>` (drop `.corrupt-<unix>`), restart. To give up on it: delete the directory; the gauge falls at the next start. `/readyz` stays 200 on purpose. A version mismatch or an I/O error is *not* quarantined: ozyd refuses to start and names the block. |
| `ozy.tsdb.corrupt_reads` climbing, `a query read a damaged block` (ERROR), queries over one time range failing | the log line names the block directory | A chunk inside a block fails its checksum. Opening does not read chunks, so this is not quarantined, and a restart does not find it either: **every query over that block's range fails until you move the block aside by hand** (`mv blocks/<ulid> blocks/<ulid>.corrupt-$(date +%s)`, restart). ozyd then treats it like any quarantined block. |
| the disk is slow (fsyncs take seconds) | `ozy.queue.lag_seconds` and `ozy.queue.lag_records` growing; acknowledged requests slow down; `ozy.tsdb.wal_syncs` | Nothing is lost and ozyd stays ready: an ack waits for the queue's fsync, and the stores' consumers wait for theirs, so the queue absorbs the backlog on disk. Fix the disk; the backlog drains by itself. A kill during the backlog loses nothing acknowledged (`slow-disk.sh`, which does not fill the queue). A full queue is a separate condition: at `pipeline.queue.max_bytes` (at least 64 MiB) intake answers 429, covered by its own tests. |
| the data volume is full | `ozy.store.broken`, `/readyz` 503, `df` | Intake refuses what it cannot keep, nothing is acknowledged and lost, `/healthz` stays 200, no panic. Free space, restart ozyd, and everything acknowledged before and after is there (`disk-full.sh`). |
| a store says it is broken | `/readyz`, `ozy.store.broken{store}` | A failed fsync stops the store until restart (see Readiness). Not drilled: no portable way to make a disk fail an fsync; the unit tests inject it. |

Not drilled: the apps' latency with ozymandias down (it needs the private app repos). A slow disk has a drill (`slow-disk.sh`, ADR-0063's notes); a
damaged block is covered by tests and not by a drill, because making one needs a block, which takes wall-clock hours to cut (see *Recovering from a corrupt block*).
See ADR-0061.

## Securing ozyd

Off by default: ozyd checks nothing and the key endpoints do not exist. To turn it on
(ADR-0059):

```console
$ export OZYMANDIAS_ADMIN_PASSWORD='something long'
$ OZY_AUTH_ENABLED=true ozyd -config deploy/ozyd.yaml
$ curl -s -X POST localhost:9400/api/v1/keys \
    -H "Authorization: Bearer $OZYMANDIAS_ADMIN_PASSWORD" \
    -d '{"name":"app-node","limits":{"requests_per_sec":50}}'
{"id":1,"name":"app-node",...,"key":"ozy_..."}      # shown once; only its hash is stored
$ OZYMANDIAS_API_KEY=ozy_... agent -config deploy/agent.yaml
```

- `/v1/*` needs the key in `X-Ozy-Key`. `/api/*` and `/debug/*` need the admin login: the UI
  shows a password form, scripts send the password as `Authorization: Bearer`. `/healthz`
  and the UI's static files stay public. An ingest key does not open `/api`.
- A wrong password, from the login form or as a bearer token, costs the attacker 300 ms at a
  single gate, so guessing is limited to a few per second. During a flood of wrong guesses a
  *new* login can answer 429; a logged-in session keeps working.
- Enabled with no password set is a startup error, so a missing variable cannot leave it open.
- Revoke with `DELETE /api/v1/keys/{id}`; it takes effect at once. `GET /api/v1/keys` lists
  keys with their last use and limits, never a secret. A lost key cannot be recovered: make a new one.
- A key's limits (`requests_per_sec`, `bytes_per_sec`, `spans_per_sec`, `log_bytes_per_day`)
  refuse with `429` and `Retry-After`. In a key, zero takes `auth.defaults` and a negative value
  (`-1`) means unlimited, to opt out of a default. Watch
  `ozy.intake.rejected{reason}`.
- What the limits count: `requests_per_sec` one per request; `bytes_per_sec` the bytes on the wire
  (the gzip'd body, or the largest body when no length is declared); `spans_per_sec` the spans that
  decoded; `log_bytes_per_day` the decoded JSON of log batches, per UTC day.
- **Series limits.** `max_series_per_metric` and `max_active_series` bound the distinct series a key
  has sent in the last hour, per metric name and in all. They answer *inside* the response: the
  request is `202`, a series past the limit is a rejection with the reason
  (`"this key already has N active series for this metric"`), counted in
  `ozy.intake.rejected{reason:series_per_metric|series_total}`, and the sender (an agent) does not retry it.
  A series already counted is never refused, an idle one ages out after an hour, and the tracker never holds
  more entries than its limit. It is process memory: after a restart it relearns from traffic. It counts what *this
  key* sent, not what the store holds, so it sits beside, not in place of, `storage.max_series_per_metric`.
  In a key, `0` means "take `auth.defaults`" and `-1` means unlimited, as for the other limits: to exempt a trusted
  key from a default series limit, give it `-1`. A refused series registers no metric name, and a series refused after
  the limit check (a type conflict) gives its slot back; a request that then fails on the queue or the store (a 5xx the
  sender retries) does not, so a failing store seeing many *distinct* series can use up a key's limit for the hour.
- **Tag rules.** When one tag is the bomb (`user_id`, a URL), drop it at intake instead of refusing the series:
  `PUT /api/v1/metrics/tag-config {"metric":"http.requests","mode":"deny","keys":["user_id"]}` (docs/api.md).
  Applied before the queue and the key's series limits, so the limit sees the merged series; counted in
  `ozy.intake.tags_dropped`. Points that now collide on a timestamp are kept first-wins and counted in
  `ozy.tsdb.ooo_rejected`. Not applied to what is stored, and exact metric names only.
- An agent whose key is wrong or revoked keeps its data (in the disk buffer if configured) and
  retries; `ozy.agent.forwarder.unauthorized` counts the refusals. Fix the key and it catches up.
- **This is not TLS.** Over plain HTTP the password, the cookie and the keys are readable on the
  wire. Put ozyd behind something that terminates TLS and set `auth.secure_cookie: true`. One
  shared password means no per-user audit trail; rotate it by restarting with a new value.
  Sessions are in memory, so a restart logs everyone out.

## Data directory

`data_dir` (default `./data/ozyd`, `/data` in the container) holds all of
ozyd's state from M1 on. The container runs as the distroless `nonroot`
user (uid 65532). The image creates `/data` with that owner, so a fresh named
volume is writable. A bind mount must be writable by uid 65532.

## Metric storage

`storage.metric_store` picks the engine. The default is `tsdb`, the real one
(M2): a write-ahead log, an in-memory head, and immutable blocks under
`data_dir/tsdb/`. `naive` selects the M1 SQLite store in
`data_dir/metrics-naive.db`, kept because it is the oracle the TSDB's
differential tests are held to — and because an operator who hits a storage
bug needs somewhere to stand while it is fixed. The two are interchangeable
through the API; nothing above the store can tell them apart.

**Distributions are stored separately, and always.** A sketch is a kilobyte of
bucket counts, not a float64, so it lives in a Pebble database under
`data_dir/sketches/` rather than in either metric store — which means
percentiles work whichever engine `metric_store` names. The four exact
aggregates of every sketch are written as ordinary series
(`<metric>.count/.sum/.min/.max`) into whichever store *is* configured, so a
distribution costs disk in both places. Retention over the sketches uses the
same `storage.retention` window, sweeps hourly, and deliberately runs one
`storage.block_range` *behind* it: the TSDB drops a whole block once its
newest sample has expired, so a `.count` outlives the cutoff by up to a block,
and a sketch expiring on the cutoff exactly would leave a window where the
count chart draws a line and `p95` returns null. Sketches with no `.count` are
unreachable rather than wrong, so the lag is the safe side to err on.

There is one transient with the same shape, and it lasts a single request: the
intake writes the four scalars first so that the append-only store decides
which buckets exist (ADR-0015), and writes the sketches for the buckets it
accepted second. Between the two writes a `.count` is queryable and its sketch
is not.

What that looks like depends on the query window, and **only one of the two
symptoms is obvious**. A window covering just the un-sketched bucket returns
null — visibly missing. A window spanning several buckets where only the
newest sketch has not landed returns a *number*, computed from the buckets
that did land: plausible, wrong, and shifted towards whatever the older data
says. That is not hypothetical — it is how this was found, as a p95 of 1408
where 1440 was right, which is the exact p95 of the earlier half of the data.

It resolves as soon as the request finishes and needs no action. It is,
though, why a test or a script that waits for `.count` before reading a
percentile is waiting on the wrong thing: the count is written first by
design, so it is true before the thing being measured exists. Wait on the
percentile settling, or on the sketch store.

Two self-metrics are worth an alert: `ozy.sketchstore.id_collisions` should be
zero forever (a non-zero value means one metric's percentiles are being
refused — the log line names both series), and `ozy.sketchstore.disk_bytes`
grows with distribution cardinality, not with traffic.

| Setting | Default | What it controls |
|---|---|---|
| `storage.metric_store` | `tsdb` | `tsdb` or `naive` |
| `storage.block_range` | `2h` | Time one block covers. See the warning below |
| `storage.retention` | `360h` (15d) | Blocks whose newest sample is older are deleted. Negative keeps everything |
| `storage.max_bytes` | `0` | Disk cap; oldest blocks deleted when exceeded. 0 disables |
| `storage.max_block_range` | `54h` | Widest a compacted block may become |
| `storage.max_series_per_metric` | `10000` | Cardinality limit per metric name |
| `storage.wal_sync_on_append` | `true` | fsync the log before the data is let go of: before the 202 in direct mode; before the consumer commits the batch's queue offset in queued mode, once per batch (ADR-0063) |
| `storage.wal_sync_interval` | `100ms` | Group-commit period when the above is off |

Each has a `OZY_STORAGE_*` environment override, e.g.
`OZY_STORAGE_BLOCK_RANGE=10s`.

### Backfill is rejected, and `block_range` decides how much

Samples are append-only (ADR-0011). Once a time range has been written to a
block, a sample inside it is refused with *"older than the oldest writable
block range"* and counted in `ozy.tsdb.ooo_rejected`.

How far back you can write is therefore roughly `1.5 x block_range` behind the
newest sample **of that series**. With the 2h default that is three hours of
slack, which no normal agent comes close to needing. With a short range it
bites immediately — a `block_range` of `10s` rejects anything more than about
fifteen seconds old, including a backfill script's first request.

**Found the hard way:** testing with `block_range: 10s`, ozyd's own
self-metrics (reported every 10s) kept the head moving, so blocks were cut
continuously and a one-off POST of two-minute-old points was half rejected.
Nothing was wrong; the store was doing what it says. Use the default range
unless you are deliberately exercising the cut path.

### What to watch

`/debug/vars` publishes, tagged `store:tsdb`:

| Metric | Why you care |
|---|---|
| `ozy.tsdb.ooo_rejected` | Samples refused as out of order or out of bounds. **Non-zero means data is being dropped** — a clock skew, a backfill, or a `block_range` too short |
| `ozy.tsdb.series_limit_rejected` | A metric hit `max_series_per_metric`. Almost always a tag carrying an unbounded value |
| `ozy.tsdb.head.series` / `.head.chunks` | The in-memory window. Grows until a block is cut, then drops |
| `ozy.tsdb.blocks` | Open blocks. Should fall when compaction runs |
| `ozy.tsdb.blocks_quarantined` | Damaged blocks set aside as `<ULID>.corrupt-<unix>`. **Not zero means a time range is missing from answers**: alert on it (see *Recovering from a corrupt block*) |
| `ozy.tsdb.corrupt_reads` | Reads that found a damaged chunk (a failed query each). Opening does not see these |
| `ozy.tsdb.disk_bytes` | Total on disk, including the log |

### Recovering from a corrupt block

**A damaged block no longer stops ozyd** (ADR-0064). At startup a block whose
`meta.json`, `index.dat` or `chunks.dat` is damaged (a bad checksum or structure, a
missing or short file, a `chunks.dat` shorter than its index says, a directory
that lost its `meta.json`) is moved aside, never deleted, and ozyd starts without
it. The rest of the store is fine.

**What you see.** One `ERROR` log line, `block quarantined: its data is not served`,
with the block, where it went (`<ULID>.corrupt-<unix>` in the same `blocks/`
directory), the cause, and `min_time`..`max_time`, the time range that is now missing.
`ozy.tsdb.blocks_quarantined` is not zero and stays so across restarts until you act.
`/readyz` stays 200 on purpose: new data is accepted and stored correctly.
**Queries over the missing range return less than was stored, and the API does not
say so.** Alert on the gauge; do not rely on noticing.

**What to do.** Find the cause first (the disk). Then either:

```
# give it up: the data in it is gone for good
rm -r data/ozyd/tsdb/blocks/<ULID>.corrupt-<unix>

# or restore it: put a good copy of the damaged file back (from a backup), take the
# ".corrupt-<unix>" off the name, and restart
mv data/ozyd/tsdb/blocks/<ULID>.corrupt-<unix> data/ozyd/tsdb/blocks/<ULID>
```

Nothing opens, cleans or expires a `.corrupt-` directory, so it can stay as long as
you like (it counts against the disk, not against `storage.max_bytes`).

**What still stops the start, and should.** A block written by a different version
of ozyd (the message names the versions): it is whole, and quarantining it would hide
good data; run the matching build. An I/O error reading a block (`EIO`, too many open
files, permissions): it says nothing about the block, which may be healthy; fix the
cause and restart. A damaged block that cannot be moved aside (a read-only volume):
ozyd will not carry on as if it had been. In all three the error names the block.

**A flipped bit inside a chunk is the case this does not catch.** Opening does not read
chunks, so the block opens; the first query that reads the damaged chunk fails with an
error and no samples (never a wrong value), `ozy.tsdb.corrupt_reads` climbs, and the log
names the block directory once. A restart does not find it either. Until you move that
block aside by hand, every query over its range fails:

```
mv data/ozyd/tsdb/blocks/<ULID> data/ozyd/tsdb/blocks/<ULID>.corrupt-$(date +%s)   # then restart
```

ozyd then treats it like any quarantined block.

A directory ending in `.tmp`, or an empty one, is the wreckage of an interrupted write
and is deleted automatically at startup. Those are expected after a hard kill and need
no action.

## Colima: UDP from the Mac doesn't reach containers

**Symptom (from M1):** a process on the Mac, such as `npm run dev` for
app-node, sends statsd to `localhost:8125`, and nothing arrives at the
containerized agent. No error is raised anywhere, because statsd is UDP and
fire-and-forget.

**Cause:** Colima's default port forwarder (`portForwarder: ssh`) forwards TCP
only. This was verified on 2026-09-19: a UDP listener in a container received
datagrams from another container, but none sent from the Mac to the published
port.

**Options:**

1. **Run the agent natively** with `make dev`. This is the simplest option,
   and matches how the host-side app itself runs.
2. **Switch Colima to its gRPC port forwarder**, which forwards UDP. Set
   `portForwarder: grpc` in `~/.colima/default/colima.yaml`, then
   `colima stop && colima start`. This restarts every container on the
   machine.
3. Run the app in a container on the `ozymandias` network and address the agent
   as `agent:8125`. Container-to-container UDP works.

TCP (the UI, `/healthz`, trace intake on 8126) is unaffected.

## Logs

Log collection is **off in `agent.yaml` and on in the compose stack** (`deploy/agent.d/logs.yaml`). With it
off the agent reads no files and opens no log streams, and ozyd's log store is
empty but present.

### What gets collected

- **Files.** A `type: file` source follows a glob. The compose stack mounts
  `deploy/app-logs/` (or `$OZY_APP_LOGS`) read-only at `/var/log/apps` and ships a source for
  `*.log` there that reads one JSON object per line. A file present when the agent starts is read from its
  end (history is not replayed); a file that appears later is read from its first line;
  `start_position: beginning` replays the first kind too.
- **Containers.** A container is followed if it carries the label `ozy.logs.enabled=true`, or a
  `type: docker` source selects it (`include_labels`, minus `exclude_names`), or
  `container_collect_all` is on. `ozy.logs.enabled=false` always wins. Labels
  `ozy.logs.{source,service,multiline_start,tags}` configure it with no agent config.
  Container discovery is a poll (`scan_interval`, 10s), not a subscription to Docker events.
- **Formats** (`source:`): `json`, `winston`, `caddy`, `python`, `rails`, `sidekiq`, `postgres`, `redis`,
  `plain`. A line no parser recognizes is kept whole as the message; one that arrived on
  stderr and no parser recognized is `status:error`. The reference for every key is the commented
  `logs:` section of `deploy/agent.yaml`.
- **The Python SDK's `JSONFormatter`** writes the `json` shape, so tracebacks are one event and `extra=`
  fields are queryable attributes (docs/sdk/python.md).

### Pipeline order and redaction

Per line: exclude, rate-limit (off unless you set `rate_limit`: it drops the excess for good, and a backlog read after an outage would look like a flood), parse, remap, **redact**. Redaction is last and on by default: JWTs,
`Authorization`/`Bearer` values, secrets in URLs (`?token=…`), `password=`/`token=`-style pairs and email
addresses become `[REDACTED]` in the message and in every string attribute, and the values of attributes
whose key contains one of a few secret words (`password`, `passwd`, `secret`, `api_key`, `private_key`,
`credential`, `cookie`, `authorization`, `phone`, `ssn`, `card_number`, `cvv`, `cvc`, and `token` or
`access_key` at the end of the key, so `csrftoken` counts and `token_count` does not), plus any
`redact.keys` you add, are replaced whatever they contain. Turn a default off with `redact.disable`, add
patterns with `redact.rules`. Each rule has a cheap necessary condition (the literal it cannot match without): `@` for email, `eyJ` for a JWT, `authorization`, a `=` and a `?` or `&` for URL secrets, and for key/value secrets a `=` or `:` *and* a secret word, each anywhere in the line (not near each other). A line that fails a rule's condition skips its regular expression. A typical JSON line with none of those costs about 3.5 µs against 14 µs before, the same as with redaction off; a line that mentions a common word (`key`, `token`, `number`) and has any `:` runs the key/value rules and costs more. The conditions read a lowercased copy built in a stack buffer, which also maps the two non-ASCII runes (the Kelvin sign and the long s) that the rules' case-insensitive matching treats as `k` and `s`. The conditions are tested against the unfiltered rules over generated strings (`TestRedactPrefilter_*`). Verify with
`make smoke` (it plants a token and an email and searches for them) and `TestAcceptance_NoSecretIsFindable…`.

### Delivery guarantee

**At-least-once.** Positions are saved in `logs.registry_path` after ozyd accepted the lines. After a crash
the agent repeats at most one batch (`logs.batch_logs`, default 1000) and skips nothing. With **no registry
path** positions live in memory: a restart starts every file over from its `start_position`. Mount a volume
at the registry's directory in any real deployment (compose does: `ozymandias-agent-data`).

### Known limits

- A Rails request still open in the pipeline when the agent dies is lost (its lines were consumed into
  the group, and the commit covers them). Other formats lose nothing.
- A line over 256 KiB is truncated. A multiline event ends at 500 lines or 256 KiB and the rest starts a new event; neither cap is configurable.
- A tail whose reader falls behind drops logs (buffer 1000, 64 tails per server) and says how many.
- A search is bounded by `logs.scan_budget`: past it the answer is a correct newest-first *prefix* with
  `truncated: true`, never a silent partial.
- Clock skew: a log stamped more than 10 minutes in the future is rejected (the response says which, and
  the agent logs it). A log older than the retention window is stored and removed with its day at the next
  sweep.
- Docker log files are read through the daemon's API, so log drivers that do not support reading
  (`none`, some remote drivers) give nothing.

### Storage

`logs.retention` (default 7d) drops whole days, so a log lives between the retention and the retention plus 24 hours. Chunk files live under `data_dir/logs/`, the WAL under
`data_dir/logs/wal`. `ozy.logstore.*` reports streams, entries, head size, raw and compressed bytes and the
bloom filters (docs/metrics-catalog.md); `raw_bytes / compressed_bytes` is the compression ratio.
`logs.no_sync: true` skips the WAL fsync: faster, and an ozyd crash can lose the last unsynced batch that the
agent already considered delivered. Leave it off. Chunk layout: [docs/formats/log-chunk.md](formats/log-chunk.md).

### Logs troubleshooting

| Symptom | Check |
|---|---|
| Nothing appears | `curl :8126/debug/vars`: `ozy.agent.logs.lines` 0 means nothing was read (`files_tailed`, `containers_tailed`: is the source matching?); `lines` rising but `emitted` flat means excluded or rate-limited; both rising means look at `forwarder` errors and ozyd's `ozy.intake.logs_rejected` |
| A file's old lines are missing | It existed when the agent first saw it, so it was read from its end. Set `start_position: beginning` |
| Lines repeat after a restart | Expected, at most one batch. Many repeats mean the registry is not persisted: set `registry_path` on a volume |
| A search is slow | `stats` in the response: `blocks_read` high with `blocks_skipped` 0 means the query has nothing a filter can test (a label, a number comparison, a negation, a word under 3 bytes). Narrow it with `service:` and `status:` |
| `truncated: true` | The scan budget ran out. Narrow the range or query, or raise `logs.scan_budget` |
| The tail shows "skipped" | The browser could not keep up; narrow the query |

## Monitors

Monitors are defined through the UI or API (docs/api.md#monitors) or as files in
`provisioning.monitor_paths` (`deploy/monitors/` ships three for ozymandias
itself, which notify `@log`). The query grammar is in
[query-language.md#monitors](query-language.md#monitors); the evaluation model
in [DESIGN.md §16](../DESIGN.md#16-monitors).

A provisioned monitor is read-only through the API; edit its file and restart.
Changing its query resets its groups (a `reset` event), as an API edit does.
**Removing its file** does not delete it. The monitor keeps running, is
*released* to the API (a `released` event and a warning in the log), and can
then be edited or deleted there. If the file comes back with the same uid,
provisioning refuses it as belonging to an API-managed monitor until that one
is deleted. Nothing is released in a pass where some file's uid could not be read: a
configured directory that is missing (not mounted this time) or unreadable,
or a file that is not JSON at all. The missing file might be one of those,
so ozyd logs which monitors it is holding back and why. A file that is
broken in any other way (a bad query, an unknown field, a uid the API now
owns) still has a readable uid. It keeps its monitor provisioned and does
not hold up releasing the others.

### Notifiers

A message's `@name` mentions select notifiers. **Every message from a monitor
goes to every handle in its template's literal text**: an `@` at the start of
the message or after whitespace. An email address is not one, and comments
and string literals inside `{{ }}` are not text. Two forms are refused at
save, with the reason:

- A mention inside `{{if}}` or `{{with}}`. The text can vary by status; the
  recipients do not.
- `@handle` directly after `}}`, as in `{{.Value}}@cap` or
  `ALERT {{- /* note */ -}} @pager`. What it reads as depends on what came
  before it (an `{{if}}` may print nothing, a `-}}` trims the space). Put a
  space before it for a mention, or write `{{"@"}}` for a literal `@`:
  `{{.Group.owner}}{{"@"}}example.com` builds an address. To page `@pager` for alerts and only log recoveries, make two
monitors. Data never changes who is notified; a tag value
that reads `@pager` is just text
([ADR-0052](adr/0052-notifiers-come-from-the-template-source.md)).
Notifiers are configured under `notifiers:` in `ozyd.yaml`:

```yaml
notifiers:
  log:         {type: log}
  hook:        {type: webhook, url_env: MONITOR_HOOK_URL, headers_env: {Authorization: HOOK_AUTH}}
  discord-ops: {type: discord, webhook_url_env: DISCORD_WEBHOOK_URL}
  me:          {type: email, to: [you@example.com],
                smtp: {host_env: SMTP_HOST, port: 587, user_env: SMTP_USER, password_env: SMTP_PASSWORD, from: ozymandias@localhost}}
```

**Secrets are only ever environment variables.** Every `*_env` field names a
variable; the value is read at startup, held in memory, never written to
`meta.db`, never returned by the API (`/api/v1/notifiers` lists names and
types), and scrubbed from delivery errors. Don't use `OZY_`-prefixed names for
them: that prefix is ozyd's config-override namespace. A notifier whose variable
is unset stays configured and fails each delivery with a `notify_failed` event
naming the variable, and ozyd logs a warning at startup, so a missing secret
shows up in the monitor's history rather than as a page that never came.

Delivery: three attempts, ten seconds each, with backoff; a `4xx` other than
`429` is not retried. Each outcome is an event on the monitor. A message with no
mention notifies nobody, though its transitions still show in the UI. To check
a notifier end to end, use **Test notification** on the monitor (or
`POST /api/v1/monitors/{id}/test-notification`), which sends now and reports
each notifier's result.

### Webhook payload

`POST` with `Content-Type: application/json`:

```json
{"monitor_id": 7, "monitor_uid": "jobs-backlog", "monitor_name": "Job queue is backing up",
 "tags": ["team:shop"], "group_key": "queue:default", "group": {"queue": "default"},
 "status": "ALERT", "prev_status": "WARN", "value": 25.5, "threshold": 20,
 "reason": "transition", "message": "…rendered message…",
 "link": "http://localhost:9400/monitors/7?from=…&to=…", "ts": "2026-10-16T12:00:00Z"}
```

`value` is `null` for NO_DATA; `reason` is `transition`, `renotify` or `test`.
A recovery is `status: "OK"` with the previous status in `prev_status`.
Delivery is **at least once**, so a receiver that must not act twice should key
on `(monitor_id, group_key, status, ts)`.

### Notification messages

Go `text/template` ([ADR-0049](adr/0049-go-templates-for-monitor-messages.md)) over
`.Monitor` (prints the name; `.Monitor.ID`, `.Monitor.UID`), `.Group` (prints
`queue:default`; `.Group.queue` is one value, empty if absent), `.Status`,
`.PrevStatus`, `.Value`, `.Threshold` (the level that was crossed: the warning
level for WARN), `.IsAlert`, `.IsWarning`, `.IsRecovery`, `.IsNoData`, `.Since`,
`.Link`:

```
Queue depth is {{printf "%.0f" .Value}} on {{.Group}}.
{{if .IsAlert}}Check the workers.{{else if .IsRecovery}}Recovered.{{end}} @discord-ops
```

A template is executed against sample data for every status when it is saved,
so a misspelt field is refused then. The example sends every transition to
`@discord-ops`, with text that depends on the status.

### Muting

Muting stops notifications and nothing else: evaluation continues and
transitions are recorded, so the monitor's page tells the truth during the
mute. With a renotify interval, the first reminder after unmuting goes out once
the interval has passed since the last notification.

### Why didn't my monitor fire?

Walk these in order:

1. **Is it running?** `/api/v1/monitors/status` should say `"stalled": false`;
   the UI shows a banner when it does not. `enabled: false` monitors are not
   scheduled.
2. **Did evaluation fail?** The monitor's events show `eval_error` with the
   reason (a query that times out, a log query past its scan budget, a metric
   store that is not configured). The state is left alone while it fails.
3. **Delay and window.** The window ends `evaluation_delay_s` (default 30 s)
   before each tick, and the tick comes every `evaluation_interval_s`. A spike
   shows up at most `delay + interval` late, and with `avg` over a long window it
   may never move the average past the threshold. **Validate** (or
   `POST /api/v1/monitors/validate`) shows each group's value right now.
4. **No data is not zero.** A metric group with no points is absent, not 0, so
   `< 1` never fires for a series that stopped. That needs `notify_no_data`,
   which fires `no_data_timeframe_m` (default twice the window) after the newest
   point, and only for a group that has had data. A group is absent only once
   its window holds no points, so a timeframe shorter than the window is
   overruled by it: on `max(last_5m)` with `no_data_timeframe_m: 3`, a stopped
   series goes NO_DATA once the five-minute window has passed its last point,
   not three minutes after it. Both are measured to the window's end, which is
   `evaluation_delay_s` before the tick, so the delay adds itself: with 30 s
   ticks and the default 30 s delay, about six minutes after the last point
   (`TestNoDataWaitsForTheWindowToEmpty`). For a heartbeat, make the
   window just longer than the beat (`last_2m` for one a minute) and let the
   timeframe decide. Counters are in between: the
   agent zero-fills a counter for `aggregator.context_expiry` (5 minutes) after
   its last event, so `errors > 0` recovers on real zeros, but a counter whose
   app stopped goes absent after those five minutes. Gauges are never
   zero-filled. Log monitors are the opposite of gauges: no matching line is a
   count of 0.
5. **Hysteresis and consecutive failures.** A group in ALERT stays there until
   it is past `critical_recovery`, and entering ALERT can need
   `min_consecutive_failures` breaching evaluations in a row.
6. **`require_full_window`** skips a group whose window has a gap, which for a
   sparse metric is every evaluation.
7. **Delivered?** `notify_ok` / `notify_failed` events say what happened to each
   notification and why; a message that mentions nobody sends nothing.
8. **Muted?** The monitor list shows a mute until its end.

## Troubleshooting

| Symptom | Check |
|---|---|
| `make up` hangs at "Waiting" | `docker compose -f deploy/docker-compose.yml logs ozyd`. A config error exits 2 with the reason on the first line |
| Exit code 2 | Configuration or usage error. The message names the file, line or key. Includes a configured `hostname` that cannot be a `host` tag (a comma, surrounding whitespace, a trailing `:`, over 195 bytes), in either binary |
| Exit code 1 | Runtime failure: port in use, unusable `data_dir`, or (agent only) no `hostname` configured and no usable OS hostname. ozyd in that case tags its own metrics `host:ozyd` and logs a warning. See the `exiting` log line |
| `/` says "built without the web UI" | Run `make web && make build`. The image build does this for you |
| `host` tag is `ozymandias-host` | Compose was started without `make up`, which exports `OZY_HOSTNAME`. Export it and recreate: `OZY_HOSTNAME=$(scripts/hostname.sh) docker compose -f deploy/docker-compose.yml up -d` |
| ozyd's `host` tag differs from the agent's, or is a container id | Run natively without `make dev`, ozyd uses the OS hostname (`name.local` on macOS). Set `hostname` in `ozyd.yaml`, or `OZY_HOSTNAME`, to the agent's name |
| A metric never appears | Walk the path in order, stopping at the first zero: `statsd.packets_received` → `messages_received` → `aggregator.contexts` → `forwarder.payloads_sent` → the metric in `/api/v1/metrics`. Each stage below names what a zero there means |
| `packets_received` is 0 | Nothing arrived. From the Mac to a containerized agent this is the Colima UDP limitation above. Otherwise check the app's `OZY_AGENT_HOST`/`PORT` and that the app and agent share a network |
| `messages_received` is 0 but packets arrived | The lines are malformed: `parse_errors` counts them, and `unsupported` counts events (`_e{`) and service checks (`_sc`), which ozymandias drops on purpose |
| `payloads_sent` stays 0 while contexts exist | The forwarder cannot reach the intake. Check `forwarder.retries` climbing, `queue_bytes` growing, and the agent's `intake_url` in `/healthz` |
| Wait ~20s before concluding anything | A value is only queryable after its 10s bucket closes and the next flush ships it. Two flush intervals is the honest upper bound |
| A series is rejected at the intake | `forwarder.series_rejected` is non-zero and the intake logs the reason. The usual cause is a type conflict: the same metric name was first seen as another type (`meta.db` keeps the first one) |
| Percentiles look wrong | They are nearest-rank over a bounded reservoir (`histogram_max_samples`), not exact. Real sketches arrive in M2 |
