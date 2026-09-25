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
stamps the image with `git describe`, and passes the machine's short hostname
to the agent's `host` tag. `make down-v` also deletes the data volume.

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
| Exit code 2 | Configuration or usage error. The message names the file, line or key |
| Exit code 1 | Runtime failure: port in use, unusable `data_dir`, no hostname. See the `exiting` log line |
| `/` says "built without the web UI" | Run `make web && make build`. The image build does this for you |
| Agent's `host` tag is `ozymandias-host` | It was started without `make up`. Set `OZY_HOSTNAME` or `OZY_AGENT_HOSTNAME` |
| A metric never appears | Walk the path in order, stopping at the first zero: `statsd.packets_received` → `messages_received` → `aggregator.contexts` → `forwarder.payloads_sent` → the metric in `/api/v1/metrics`. Each stage below names what a zero there means |
| `packets_received` is 0 | Nothing arrived. From the Mac to a containerized agent this is the Colima UDP limitation above. Otherwise check the app's `OZY_AGENT_HOST`/`PORT` and that the app and agent share a network |
| `messages_received` is 0 but packets arrived | The lines are malformed: `parse_errors` counts them, and `unsupported` counts events (`_e{`) and service checks (`_sc`), which ozymandias drops on purpose |
| `payloads_sent` stays 0 while contexts exist | The forwarder cannot reach the intake. Check `forwarder.retries` climbing, `queue_bytes` growing, and the agent's `intake_url` in `/healthz` |
| Wait ~20s before concluding anything | A value is only queryable after its 10s bucket closes and the next flush ships it. Two flush intervals is the honest upper bound |
| A series is rejected at the intake | `forwarder.series_rejected` is non-zero and the intake logs the reason. The usual cause is a type conflict: the same metric name was first seen as another type (`meta.db` keeps the first one) |
| Percentiles look wrong | They are nearest-rank over a bounded reservoir (`histogram_max_samples`), not exact. Real sketches arrive in M2 |
