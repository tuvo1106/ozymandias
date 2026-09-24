# ozy (Node.js SDK)

A zero-dependency extended-StatsD-compatible metrics client for [ozymandias](https://github.com/tuvo1106/ozymandias),
a self-hosted observability stack. Node ≥ 22, ESM and CommonJS, TypeScript types included.

```ts
import { init, statsd } from "ozy";

init({ service: "checkout", env: "dev", version: "1.4.0" }); // OZY_* env vars fill the rest

statsd.increment("http.request.count", 1, { tags: ["route:/api/items", "method:get"] });
statsd.distribution("http.request.duration", 12.4, { tags: ["route:/api/items"] });
statsd.gauge("queue.depth", 3);
const out = await statsd.timed("image.resize.duration", () => resize(buf));
```

- **Inert unless configured:** without `OZY_AGENT_HOST` every call is a no-op and no
  socket, timer or process hook is created.
- **Never throws into your app:** failures are swallowed and counted in `statsd.stats()`.
  `timed()` rethrows *your* function's errors unchanged.
- **Never blocks:** calls append to an in-memory buffer; datagrams go out over UDP at most
  100 ms later, coalesced up to 1432 bytes.

## Install

The package is not on the npm registry yet. Build a tarball from a checkout and install that:

```sh
cd sdk/node && npm ci && npm pack        # → ozy-0.1.0.tgz
cd /path/to/your-app && npm install /path/to/ozy-0.1.0.tgz
```

## Configuration

| Env var | `init()` option | Default | Meaning |
|---|---|---|---|
| `OZY_AGENT_HOST` | `agentHost` | unset | Agent host or IP. **Unset → SDK disabled.** |
| `OZY_STATSD_PORT` | `statsdPort` | `8125` | Agent extended StatsD UDP port |
| `OZY_SERVICE` | `service` | unset | Sent as `service:` tag |
| `OZY_ENV` | `env` | unset | Sent as `env:` tag |
| `OZY_VERSION` | `version` | unset | Sent as `version:` tag |
| `OZY_TAGS` | `tags` | none | Comma-separated tags added to every metric |
| `OZY_DEBUG` | `debug` | off | Log SDK events to stderr (`1`/`true`/`yes`/`on`) |

`init()` options win over the environment. The full guide (API reference, formatting and
sampling rules, safety guarantees, troubleshooting) is
[docs/sdk/node.md](https://github.com/tuvo1106/ozymandias/blob/main/docs/sdk/node.md); the wire
format is [docs/wire-protocol.md §A](https://github.com/tuvo1106/ozymandias/blob/main/docs/wire-protocol.md).

## Development

```sh
npm ci
npm run typecheck && npm run lint && npm run test:coverage   # 90% coverage gate
npm run build                                                 # dist/esm + dist/cjs
```

## License

MIT — see [LICENSE](LICENSE).
