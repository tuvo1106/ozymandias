# ADR-0048: Monitor state lives in SQLite, and notifications go through an outbox

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Alerting must survive a restart without announcing an alert twice or losing
one. It must not notify for a state it failed to record, and a slow or broken
notifier must not delay evaluation. Something has to hold each group's state,
when it was last notified, and the notifications still owed.

## Decision

`monitor_states` (one row per monitor and group), `events` (the history) and
`notify_outbox` live in the existing metadata database (`meta.db`, SQLite).
One evaluation of one monitor commits its state rows, events and outbox rows in
**one transaction**, and the scheduler adopts the new states in memory only
after that commit. A dispatcher delivers from the outbox, one queue and worker
per notifier, and deletes a row in the same transaction that records its
outcome. Delivery is therefore at least once. Write transactions are serialized
in-process.

## Alternatives considered

| Option | Why not |
|---|---|
| State in memory, rebuilt by re-evaluating at startup | A restart mid-ALERT re-enters ALERT from OK and announces it again, and renotify timers reset. |
| Send, then persist | A crash or write failure after sending leaves state that disagrees with what was said; the next evaluation repeats the message. |
| Persist, then send directly (no outbox) | A crash between commit and send loses the notification for good, the one failure alerting cannot afford. |
| Pebble (as the trace store) | The data is small, relational (filter events by monitor and time) and written with the definitions it belongs to; SQLite is already there for exactly that. |

## Consequences

Each evaluation is a transaction: 200 monitors × 50 groups every 30 s is
about 330 state-row writes a second (the L6 test runs that load, under
`-race`; its throughput is not yet measured as a benchmark). A crash between a
successful POST and its completion delivers twice; that is chosen over never.
The outbox survives a notifier outage of any length; a notifier removed from
config makes its rows fail with a reason.
