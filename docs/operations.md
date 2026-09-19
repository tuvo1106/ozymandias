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

## Health and self-metrics

```console
$ curl -s localhost:9400/healthz
{"component":"ozyd","status":"ok","uptime_seconds":42,"version":"v0.1.0"}
$ curl -s localhost:8126/debug/vars       # the agent's ozymandias.* metrics
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
