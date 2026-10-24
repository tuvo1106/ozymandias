# ADR-0053: M6 ships monitors for app-python only

- **Status:** Accepted
- **Date:** 2026-10-23

## Context

M6's spec (§5) lists shipped monitors for three apps and for ozymandias itself. Two of those apps
are not integrated: ADR-0037 parked app-ruby and app-node in M3, and nothing since has onboarded
them. Their monitors would name metrics nobody sends (app-node's disk gauge, app-ruby's scraped
`queue_depth` and `quality_breaches`), so they would validate, never see a group, and could not be
demonstrated. The ozymandias self-monitors shipped in #58, all but the WAL fsync one (there is no
such metric yet; `docs/notes/M6.md` says so). On 2026-10-23 the owner chose app-python only.

## Decision

**`deploy/monitors/` gets the six app-python monitors of §5 and none for app-node or app-ruby.**
Each was dry-run against live app-python data before it was committed, and the M6 acceptance runs
(backlog flood, worker stop, ozyd restart, injected errors) use them.

Two follow the spec in substance but not in letter, because the spec's text was written before the
app existed:

- `service:app-python-api` is `service:shikomi-api` and `service:app-python-worker` is
  `service:shikomi-worker`, the names the app reports. `deploy/` is where app-specific names live.
- The 5xx rate is written `(all - non_5xx) / all`, not `5xx / all`: a selection that matches
  nothing is no series (query-language.md), so the plain ratio has no value while the app is
  healthy and the monitor would sit in "no data" exactly when all is well. The complement is 0.

## Alternatives considered

| Option | Why not |
|---|---|
| Ship all three apps' monitors from the spec | Two thirds of them could never be checked against data; a shipped monitor that has never seen a group is the silent emptiness AGENTS.md warns about. |
| Ship them disabled | Same problem, later: nobody would notice they are wrong until the app is onboarded, which is when they should be written. |

## Consequences

- Onboarding app-node or app-ruby (the follow-up ADR-0037 describes) now also means writing its
  monitors from §5 and dry-running them on its data.
- The spec's §5 bullets for those two apps stay as the plan for that work; this ADR amends §5 to
  say so.
