# Examples

Small, runnable programs that use ozymandias without being one of the owner's apps. Each one is
inert with `OZY_AGENT_HOST` unset and traced with it set.

| Example | What it proves | Run it (with the agent up, e.g. `make dev`) |
|---|---|---|
| [`go-app`](go-app) | A program with no SDK can post spans to the agent (wire-protocol §B) | `go run ./examples/go-app` |
| [`flask-app`](flask-app) | The Python SDK on WSGI: the `flask` integration, one manual span, a log line carrying `trace_id` | `cd examples/flask-app && OZY_AGENT_HOST=localhost uv run app.py --drive` |
| [`express-app`](express-app) | The Node SDK on Express: `traceRequests`, `instrumentFetch` to a second service, `traceErrors` | `cd examples/express-app && npm install && OZY_AGENT_HOST=localhost node server.mjs --drive` |
| [`cron-script.sh`](cron-script.sh) | A shell script reporting a metric over UDP | see the file |

`--drive` serves on a free port, sends six requests (200, 200, 200, 404 from a handler, 404 with no
route, 500) inside one trace, and prints that trace's id: open `/apm/traces/<id>` in the UI. These are
not part of `make ci`; they need a running agent, so they are checked by hand (see `docs/notes/M5.md`).
