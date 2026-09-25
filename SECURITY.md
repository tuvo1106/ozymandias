# Security policy

ozymandias is a learning project. It is not hardened, it has no authentication
before M7, and it should not be exposed to a network you do not control. With
that said, a security bug here is still a bug worth knowing about.

## Reporting

Please report privately rather than in a public issue: use
[GitHub's private vulnerability reporting](https://github.com/tuvo1106/ozymandias/security/advisories/new)
on this repository. Expect a first response within a week. There is no bounty.

Include what you did, what happened, and the commit you were on. A failing test
is the most useful possible report.

## Scope

In scope: `ozyd`, the agent, the SDKs under `sdk/`, and the wire protocol in
`pkg/wire` — in particular anything that lets untrusted input crash a process,
escape its data directory, or corrupt a block or the WAL.

Out of scope, because they are known and documented rather than accidental:

- **No authentication or TLS.** The HTTP API and the StatsD listener are open
  to anyone who can reach the port. API keys and limits arrive in M7
  ([`docs/plan/M7-hardening.md`](docs/plan/M7-hardening.md)).
- **No multi-tenancy.** Any caller can read any series.
- **Resource exhaustion by design.** Series and cardinality limits are
  configurable and default to generous; a determined writer can fill the disk.
- Anything in `docs/plan/` that is not implemented yet.

## Supported versions

Only `main`. There are no releases.
