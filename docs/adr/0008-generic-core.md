# ADR-0008: A generic core: apps are configuration, never code

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

app-node, app-python and app-ruby are the first users, not the only ones. app-ruby (Ruby) deliberately has no ozymandias SDK, to prove an unknown app can be onboarded.

## Decision

No code under `cmd/`, `internal/`, `pkg/`, `sdk/` or `web/src/` names a specific app, enforced by `scripts/check-no-app-coupling.sh` in `make lint`. App-specific material lives only in `deploy/agent.d/`, `deploy/dashboards/`, `deploy/monitors/`, `docs/private/integrations.md` and the apps' own repos. An app's needs are met by generic mechanisms: conf.d fragments, container-label autodiscovery, OpenMetrics scraping, extended StatsD compatibility and OTLP.

## Alternatives considered

| Option | Why not |
|---|---|
| Special-case each app where convenient | The resulting tool works only for its author's apps, and the extensibility lesson is lost. |

## Consequences

Some app needs cost more to meet generically (a configurable rewrite rule instead of an `if`). docs/plan/extensibility.md holds the full standard.
