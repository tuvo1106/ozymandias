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
- The 5xx rate and the latency p95 are **rolled up** so that each bucket is a whole ratio or a
  merged sketch. A window reduction applies to the expression's per-bucket values, so
  `sum(last_5m)` of a ratio on ten-second buckets adds thirty percentages (1% errors read as 30),
  and `avg(last_5m)` of a p95 averages per-bucket percentiles, so one slow request on a quiet route
  is the whole answer.
  - **5xx:** `.rollup(sum, 120)` on both terms, `max(last_6m)`. Rolled-up buckets are fixed
    (epoch-aligned), so one five-minute bucket would split a burst that straddles its boundary
    into two diluted halves. With two-minute buckets and the max of the last three, a burst of
    four minutes (twice the bucket) or more fills a whole bucket wherever it falls, and one of two
    to four minutes still puts at least half its rate into one bucket. A bucket closes within two
    minutes. `TestShippedErrorRatesReadWholeRatios` evaluates the shipped file with bursts swept
    across every ten-second offset of a bucket.
  - **No minimum traffic.** On a quiet stack, one 5xx among ten requests in two minutes reads 10%
    and alerts. That is the error rate those requests had; the grammar has no "and at least N
    requests" gate, and a smoothed ratio (`5xx / (all + k)`) would change what the number means.
    A minimum-volume option on monitors is the fix if it proves noisy.
  - **Latency:** `.rollup(max, 300)`, `last(last_5m)`: one merged five-minute p95. A shorter
    regression can be split across two buckets and missed. A p95 monitor is for sustained
    regressions, and the five-minute sketch is what keeps one slow request from paging.
- **Every monitor is split by environment** (`by {env}`, or `.by("@env")` for the log monitor,
  whose logs carry `env` as an attribute rather than a tag). Only `dev` exists today, but a
  merged `max` would let a live dev worker hide a dead prod one, and a merged ratio would dilute
  prod's errors with dev's traffic. The exception is sandbox OOM: judge sandboxes carry no env or
  compose labels.
- **The OOM monitor recovers because its counter is zero-filled.** `container.exits` goes through
  the agent's aggregator as a counter, zero-filled for `context_expiry` (5 minutes), which is
  longer than the monitor's 3-minute window. A window longer than the zero-fill would leave an OOM
  alert stuck once the series went absent.
- **A quiet group keeps its status.** A per-resource latency group that alerts and then gets no
  traffic has no value, and an absent group keeps its status (state/doc.go) until it expires after
  24 hours. That is the state machine's rule (absence is not recovery), and `notify_no_data` here
  would page on every quiet route. The same holds for the 5xx monitor if the api goes completely
  silent after an outage, which is not a recovery either.
- The 5xx rate stays the **direct** ratio `5xx / all`, as docs/dashboards.md says for error rates.
  Healthy, it has no series, and a group that never had data never becomes NO_DATA, so it just
  never alerts; during a total outage it reads 100, where the complement `(all - non_5xx) / all`
  would read nothing. (An earlier draft of this ADR chose the complement for a reason that was
  wrong.)

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
