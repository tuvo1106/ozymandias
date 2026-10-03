# ADR-0034: A series missing a `by` key is grouped under its absence

- **Status:** Accepted
- **Date:** 2026-10-03

## Context

The M3 plan (§1, step 6) says a series missing a `by` key goes into the group
`"N/A"`. The evaluator does not do that:
`groupOf` leaves the key off the group's tags, so `sum:m{*} by {route,env}` over
series where only some have `env` returns lines scoped `route:/x` rather than
`route:/x,env:N/A`. `docs/query-language.md` and a test (`a series missing a
group-by key groups under no value for it`) say so. The plan was never amended
and there was no ADR, which `AGENTS.md` asks for when a spec is wrong. This
records it after the fact, so the original reasoning is not on record; what
follows is what the behaviour buys.

## Decision

**A series that lacks a `by` key is grouped under that key's absence, and the
key is left off the group's tags.** `docs/query-language.md` is normative; the
plan's `"N/A"` is superseded.

## Alternatives considered

| Option | Why not |
|---|---|
| The group `"N/A"` (the plan) | `N/A` becomes a tag *value* the index has never seen: a real value spelt `N/A` would merge with it, and a scope containing `key:N/A` is not a filter that matches any series. |
| Drop series missing the key | A grouping that silently loses data is the failure the milestone's own review rounds kept finding. |

## Consequences

A line's scope contains only tags that exist, so it can be pasted back into a
filter. The cost is that a missing
key is visible only as a shorter scope: `by {problem_difficulty}` over a metric
without that tag returns one line scoped `*`, which looks like "no grouping" and
not like "nothing has this tag". The M3 notes record the case.
