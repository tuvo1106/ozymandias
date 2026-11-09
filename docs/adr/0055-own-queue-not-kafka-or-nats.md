# ADR-0055: Own single-node queue on the WAL, not Kafka or NATS

- **Status:** Accepted
- **Date:** 2026-11-08

## Context

M7 decouples intake from storage with a durable log (M7 §1). Intake answers
`202` once a request is durable in the log; per-store consumers apply it at
their own pace and commit offsets; a crash redelivers from the committed
offset. Something has to be that log. ozymandias is a single-node, two-binary
system deployed by `docker compose`, and its point is to teach how these parts
work (AGENTS.md §1).

## Decision

Write the queue: `internal/queue`, one directory per topic, reusing the
`internal/tsdb/wal` segment framing, CRC and torn-tail recovery unchanged.
Each record carries its own offset and produce time. Group commit (one syncer
goroutine per topic; 5 ms window or 1 MiB) makes durability cheap enough to
pay on every request. Consumers commit by atomically replacing a 12-byte
offset file. Format: `docs/formats/queue-segment.md`.

Three behaviours decided with it:

- **A full topic refuses, it does not evict.** At `max_bytes` `Produce`
  returns `ErrFull`. A record in the queue has been acknowledged, so deleting
  it unconsumed would be silent loss of exactly the kind the queue exists to
  prevent. Intake sheds load (429) well before the cap (M7 §2).
- **A failed fsync stops the topic until restart.** After a failed fsync the
  kernel may have discarded the dirty pages and a later fsync can succeed
  without them (PostgreSQL's 2018 "fsyncgate"). Retrying would acknowledge
  data that is not on disk.
- **Consumers read only below the synced end.** Otherwise a store could apply
  a record the queue then loses in a crash, and the unacknowledged producer
  would resend it under a new offset.

The spec's optional NATS JetStream implementation behind the same interface
is not built.

## Alternatives considered

| Option | Why not |
|---|---|
| Kafka (or Redpanda) | A JVM (or a second large service) and a broker to operate, for one producer and four consumers on one machine. It would also hide the mechanism M7 is meant to teach: segments, offsets, group commit, retention by committed offset. |
| NATS JetStream | Lighter, but still a second service and a client dependency outside the allowlist (AGENTS.md §4), and the interesting parts (its file store, acks, redelivery) would be configuration rather than code. Kept as the spec's optional exercise. |
| SQLite table as the queue | Already a dependency, and transactional, but every record becomes a B-tree insert plus a delete at commit, and the WAL-mode checkpointing competes with the meta DB. An append-only log is the shape the workload has. |
| Pebble as the queue | Ordered keys by offset would work, but a log-structured merge tree pays compaction to delete what a segmented log deletes by unlinking a file. |
| Evict the oldest records at the cap | Keeps intake accepting under a long store outage, at the price of losing acknowledged data silently. Refusing makes the outage visible to senders, whose own buffers (the agent's disk buffer, M7 §3) are where waiting belongs. |
| Retry a failed fsync | Looks resilient and is the bug fsyncgate described. |

## Consequences

- One more on-disk format with a golden file, and a per-store idempotency
  obligation: at-least-once delivery means every consumer must tolerate
  redelivery (M7 §1; PR 2 of M7).
- Open reads every retained record once. A topic is normally near-empty;
  after a long store outage startup is proportional to the backlog. Measured
  in `docs/notes/M7.md`.
- Ack latency gains a floor of the group-commit window on an idle topic;
  `queue.sync_interval` is the knob, and `pipeline.mode: direct` keeps the
  pre-M7 path for comparison.
- A single node only. Replication, partitions and consumer groups are out of
  scope; the interface (Produce / Poll / Commit) is the seam a replicated log
  would sit behind.
