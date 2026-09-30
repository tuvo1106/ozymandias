# ADR-0031: The postgres check uses pgx

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

M3 §3 ships a `postgres` check: connections, commits and rollbacks, tuple
activity, the buffer cache hit ratio, and database, table and index sizes.
All of it comes from SQL (`pg_stat_activity`, `pg_stat_database`,
`pg_database_size`, `pg_table_size`), so the agent needs to speak the
Postgres wire protocol, including authentication. Current servers default
to SCRAM-SHA-256 and many deployments require TLS.

The dependency allowlist (AGENTS.md §4) names `github.com/jackc/pgx/v5` for
exactly this, "recommended, needs its ADR". Everything else in the agent
that talks to a service is written by hand (the Docker API over the socket,
RESP for redis, the OpenMetrics parser), because writing those is part of
what this project is for.

## Decision

The postgres check connects with pgx (`pgx.Connect`, one connection per
run, closed afterwards) and nothing else in the repository imports it. The
check's SQL sits behind a small interface, so its logic — rates across
runs, the hit ratio, skipping template databases, what an unreachable server
reports — is tested without a server, and pgx stays confined to one file.

## Alternatives considered

| Option | Why not |
|---|---|
| Write the wire protocol by hand | Startup, SCRAM-SHA-256 (with SASL framing), TLS negotiation, and the simple query protocol's row decoding, all before the first metric. It is a large, security-sensitive piece whose lessons are about authentication, not observability, and a mistake in it leaks a password |
| `github.com/lib/pq` | In maintenance mode; its own README points to pgx. Not on the allowlist either |
| Defer the check | Postgres is what the reference apps run on, and the spec lists it; the openmetrics check could cover it only through a separate exporter process |

## Consequences

- One new module tree (pgx and its `pgservicefile` and `pgpassfile`
  dependencies) in the agent binary, used only by this check.
- A connection per run costs a TCP handshake and authentication every
  interval (15s by default). That is negligible for one server, and it means
  the check holds no connection between runs: a server restart or a
  failover costs nothing to recover from.
- The password is in the agent's config (or a container label, for
  autodiscovery). The check never logs it; pgx's own error messages leave it
  out.
