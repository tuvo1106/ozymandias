# ADR-0042: Span times are unix microseconds

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

Metrics are in seconds and logs in milliseconds. A span's `start` and `duration` need a resolution
fine enough for a SQL call (often under a millisecond) and a range wide enough for a job that
runs for hours.

## Decision

`start` and `duration` are integers in microseconds: `start` unix, `duration` from the monotonic
clock. A `start` below 10^15 is refused as "looks like seconds or milliseconds", which catches
the unit mistake at the boundary instead of storing spans in 1970. Index keys store `^start`
(bitwise inverted) so a forward scan is newest first.

## Alternatives considered

| Option | Why not |
|---|---|
| Milliseconds, like logs | A 300 µs query becomes 0 or 1 ms; flame graphs of fast code lose their shape. |
| Nanoseconds | Python's `time.time_ns` and JS doubles cannot both carry them exactly (a double holds integers to 2^53, and unix nanoseconds are 1.8 x 10^18); microseconds (1.8 x 10^15) fit. |
| Floating-point seconds | Rounding differs per language; an integer has one spelling. |

## Consequences

API query windows stay in milliseconds (shared with the logs API) while trace and span objects carry
microseconds; the API reference says which is which.
