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
| `docker` | `collectors.docker` | `container.*`, `docker.containers.running`, and from the event stream `container.exits`, `container.lifetime` | Every container on the daemon behind `collectors.docker.socket`. In compose that is the Docker VM's daemon, reached through the mounted socket and the socket's group, which `make up` looks up (ADR-0028); `docker compose up` by hand gets gid 0 and, under Colima, permission denied — logged once, the rest of the agent unaffected. `make dev` uses the docker context's socket. Name many short-lived containers as one with `container_name_rewrite` (the app-python fragment does, for its judge sandboxes) |

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
with it. The name is the one after `container_name_rewrite`. Containers that rewrite
folds into one name are each checked, and their metrics carry a
`replica:<n>` tag (the lowest number free among that name's running
replicas) so that they do not overwrite each other; they share one set of
`ozy.agent.collector.*` self-metrics. Sum or average across `replica` at
query time, as the metric calls for.

`%%host%%` is the container's IP address on a network, so **the agent must
share a Docker network with the container** — in compose, list the app's
network on the agent service (or the app joins `ozymandias`).
`collectors.docker.autodiscovery_network` names that network (the shipped
config says `ozymandias`); unset, a container must be on exactly one
network, and one on several is refused, since its other addresses may have
no route from the agent.

A label value reaches a setting as plain text that the check decodes:
`"6379"` is a number where the setting wants one, and a text setting gets
the value exactly as written (a password `0123` or `pa ss #1` stays as it
is). A list is written in brackets, `"[200, 301]"`. Labels that do not make
a valid check are logged once and counted in
`ozy.agent.autodiscovery.errors`. Settings are resolved again every sync: a
container restarted with a new address gets its check rebuilt, and one
that failed for want of an address is tried again once it has one.

#### Check: openmetrics

Scrapes a Prometheus or OpenMetrics `/metrics` page every run. Counters become
per-second rates (sent as gauges, ADR-0026), gauges stay gauges, histograms
become per-bucket counts `<name>.bucket` tagged `upper_bound` plus `.sum` and
`.count`, and summaries a `quantile`-tagged gauge plus `.sum` and `.count`.
Every label becomes a tag. `openmetrics.up` and `openmetrics.scrape_duration`
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
| `max_series` | 2000 | Metrics one scrape may emit; the rest are dropped and the run reports an error |
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
`system.processes.*`, tagged `process_name:<label>`. Exactly one of:

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
| `storage.wal_sync_on_append` | `true` | fsync the log before acknowledging an intake request |
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
| `ozy.tsdb.disk_bytes` | Total on disk, including the log |

### Recovering from a corrupt block

A block that fails its checksum stops startup, loudly, naming the directory:
serving a database with a silent hole in it is worse than not starting. The
data in that block is gone; the rest is fine. Move the directory aside and
restart:

```
mv data/ozyd/tsdb/blocks/<ULID> /tmp/
```

A directory with no `meta.json`, or one ending in `.tmp`, is the wreckage of an
interrupted write and is deleted automatically at startup — those are expected
after a hard kill and need no action.

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
