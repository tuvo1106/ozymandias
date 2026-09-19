# Metrics catalog

Every metric ozymandias emits about itself, and, from M1, every metric the
instrumented apps' integrations emit. `scripts/check-docs.sh` fails CI if a
metric registered in ozymandias's code is missing here.

Conventions ([integrations.md §4](plan/integrations.md#4-metric-naming-conventions-sdk-emitted-metrics)):
names are `lower.dotted`, units go in this table rather than in the name, and
tags come from bounded sets only.

## ozymandias self-metrics (`ozymandias.*`)

Readable at `GET /debug/vars` on both binaries. From M1 the agent also ships
them through its own pipeline.

| Metric | Type | Unit | Tags | Emitted by | Meaning |
|---|---|---|---|---|---|
| `ozy.build.info` | gauge | — | `component`, `version` | ozyd, agent | Always 1. The tags say which build is running |
| `ozy.process.uptime_seconds` | gauge | seconds | `component` | ozyd, agent | Time since the process started |
| `ozy.http.requests` | counter | requests | `component`, `route`, `status_class` | ozyd, agent | HTTP requests served. `route` is the matched pattern (e.g. `GET /healthz`), never the raw path. Unmatched requests are tagged `route:unmatched` |
