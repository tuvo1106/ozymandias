# ADR-0037: M3 closes on app-python alone

- **Status:** Accepted
- **Date:** 2026-10-04

## Context

M3's acceptance has two boxes that name more than one app. The first needs app-ruby onboarded with
no application change, charted against its Grafana board, with a small-order p90 wait compared via
`histogram_quantile()` and the sketch path. The second says no app-python or app-node metric
exceeds 500 series. On 2026-10-03 the owner narrowed M3 to one integration, app-python (now
shikomi), and parked app-ruby and app-node. Everything else in M3 is built, tested and
demonstrated (`docs/notes/M3.md`), and `container.exits` growth, the one cardinality risk found,
is bounded by ADR-0036.

## Decision

**M3 is closed on app-python alone.**

- The app-ruby box is **moved out of M3** to a follow-up with no date, started when the owner
  chooses to onboard that app. Its scope is unchanged: `deploy/agent.d/app-ruby.yaml`,
  `deploy/dashboards/app-ruby.json`, the Grafana comparison and the p90 two-path check.
- The Metric Summary box is **reworded to what was measured**: app-python's metric families peak at
  18 series against the 500 limit. The same check on app-node is part of onboarding app-node, whenever
  that happens.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep M3 open until app-ruby and app-node are integrated | Nothing in M3 is left to build; the open boxes need other repos' work the owner has deferred. Holding the milestone open blocks M4 on app work rather than on what M4 depends on. |
| Tick the boxes as they stand | They were not demonstrated. `AGENTS.md` says to report skipped criteria plainly. |

## Consequences

- Later milestones that lean on app-ruby carry the same debt: **M4's** acceptance mentions its
  plain-text Rails logs, and M3's `histogram_quantile()` and histogram-to-sketch path has been
  unit-tested but **never run on a real app** (app-ruby was the planned source). M4 must either
  onboard app-ruby first or amend its own acceptance, in its own ADR.
- The cardinality discipline for app-node is unverified until it is integrated.
