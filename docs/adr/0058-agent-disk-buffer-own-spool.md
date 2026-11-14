# ADR-0058: The agent's disk buffer is its own small spool, not the queue

- **Status:** Accepted
- **Date:** 2026-11-13

## Context

M7 §3 wants the agent's retry queue to spill to disk: memory first, overflow to
disk, replay oldest first after an outage or restart, drop-oldest at a cap, with a
drain priority of series, sketches, logs, traces. `internal/queue` (ADR-0055) is a
durable FIFO already, with CRC'd segments, torn-tail recovery and a cap.

## Decision

Write `internal/agent/spool` (on the same WAL framing) instead of embedding the queue.

The queue does not fit in two ways that matter. Its cap *refuses* (`ErrFull`) by
design, because a record in the intake queue has been acknowledged; the agent's
buffer must *evict the oldest*, because nobody has been promised anything and
after an outage the recent past is what matters. Making the queue evict would mean
deleting segments under consumers whose cursors point into them, which is exactly
the family of races that took four review rounds in PR 1. And the queue's
machinery (consumer registry, offset files, group commit, sync-failure latch) is
there for several consumers and for acknowledgement; the agent has one consumer, no
producer to acknowledge, and a send loop that already retries.

The spool is the single-consumer special case:

- one cursor (segment, offset), replaced atomically and not fsynced;
- whole segments are the unit of deletion, which makes drop-oldest one unlink;
- payloads are fsynced when written, the cursor is not, so a crash of the
  *machine* can re-send what was already delivered (never lose it), and a crash of
  the process loses nothing;
- a damaged record ends its segment and the spool moves on, so one flipped bit
  cannot hold every later payload hostage.

**Logs are not in it.** Logs never queue in the agent: the tailer sends synchronously
and commits its file offset after a `2xx`, so the file is the buffer (M4). The spec's
priority list names logs, but there is nothing to prioritize; a full disk buffer
cannot drop a log line because log lines never enter it. Statsd and traces are shed
(UDP drops; spans go through the spool and its cap).

**Priority** is by endpoint: series, then sketches, then spans. Within an endpoint
the spool's oldest payload goes first.

## Consequences

- Delivery from a spool is at-least-once: a payload can be sent twice around a
  machine crash. Every intake path tolerates it (TSDB no-ops a repeated sample,
  sketches and spans overwrite by key, queued intake dedupes in its stores).
- A payload in backoff at the head of an endpoint's spool holds back the newer
  ones of that endpoint. During an outage that is moot (everything is failing),
  and it keeps order. Payloads still in memory keep the old rule: a payload in
  backoff does not hold back the ones behind it.
- A failed fsync stops a spool for good until restart (fsyncgate); the forwarder
  then treats that endpoint as memory only and `spool_enabled` reads 0.
- The spool's cap is per endpoint, so the three together can use 3 × `max_disk_bytes`.
- Spill writes (with fsync) happen under the forwarder's lock, only on overflow and
  at shutdown, so a stalled disk can stall `Submit` then. Reading the spool and
  moving its cursor, the drain side, happen outside the lock. Moving the spill write
  off the lock is a later change if it shows.
- The cursor is written every 8 deliveries, not every one: a kill repeats at most 8.
