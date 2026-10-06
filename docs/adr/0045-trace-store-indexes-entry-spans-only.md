# ADR-0045: The trace store indexes entry spans, with a merge-operator service map

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

APM lists requests (slow, failing, one route's), not spans. A trace of 40 spans has one entry span
per service. Indexing every span would multiply index size by the fan-out for no question anyone
asks, and the service map needs an aggregate no span holds.

## Decision

Pebble holds the spans by `(trace_id, span_id)` and narrow indexes over `_top_level` spans only: by
service and time, by service, resource and time, and errors by service and time (an entry span
whose own trace has an error span in the same request). Time is stored inverted so scans are newest
first. The service map is a counter per `(hour, env, parent, child)` updated with a Pebble merge
operator as spans arrive; a child whose parent has not arrived waits in a bounded 60-second map and
is counted as unresolved if the parent never comes. An edge is counted only when its entry key is
new, so a resent batch does not double it. Retention is by trace through a first-seen index.
`docs/formats/tracestore-keys.md` has the layout. Two deviations from the plan's first sketch: the
edge key carries `env` (the API filters by it), and the resource index hashes the lower-cased
resource, because metric tags are lower-cased and a service page links back by that text.

## Alternatives considered

| Option | Why not |
|---|---|
| Index every span | Index size grows with fan-out; no listing question needs it. |
| Join spans at read time for the map | Reads scale with stored spans; and sampled spans make the join an estimate anyway. |
| SQLite | Fine for lookups, poor at the newest-first range scans and counters at span volume, and Pebble is already a dependency (ADR-0005). |

## Consequences

"Errors only" on a list is approximate across requests: a failing span that arrives in a different
request from its entry span does not mark the entry. Edges are counted from stored spans, so under
sampling they show shape, not rate; the service table (from metrics) is the source of truth for rates.
