# ADR-0051: A log monitor groups by at most one field

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The M6 grammar writes `.by(" keys ")`. The log store's histogram
(`LogStore.Aggregate`) groups by one field, either a reserved label or an
`@attribute`, and caps distinct values at 100.

## Decision

`.by("field")` takes exactly one field; a second is a parse error naming the
limit. Logs without the field are grouped as `(none)`. More than 100 distinct
values in one evaluation is an evaluation error rather than a `(other)` group,
because which values land in `(other)` depends on scan order, and a group that
moves in and out of it would alert and recover on alternate evaluations.

## Alternatives considered

| Option | Why not |
|---|---|
| Accept several and group by the first | A monitor that silently ignores part of what it was told is wrong where nobody looks. |
| Extend `Aggregate` to compound keys now | Changes a store API and its cardinality bound for a need no shipped monitor has. Revisit when one does. |

## Consequences

`by service and status` is written as one monitor per status, or as a metric
monitor on a counter the app emits.
