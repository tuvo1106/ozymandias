# ADR-0039: The log store is index-light, and chunk files are the source of truth

- **Status:** Accepted
- **Date:** 2026-10-04

## Context

M4's plan (`docs/plan/M4-logs.md` §3) sketches a Loki-style store: logs grouped into streams by a
small label set, buffered in a head block, sealed into compressed chunk files, found by label
index then scan. Building it turned up four places where the plan's sketch was not enough or was
wrong.

## Decision

**1. Streams are label sets of five low-cardinality labels: `service`, `source`, `host`, `env`,
`status`.** Status is a label because the question people ask first is "just the errors", and an
index that cannot answer it forces a scan of every log. It is safe as a label because it has five
values. `trace_id` is not a label (it is the highest-cardinality field a log has) and is always a
scan. The planner (`logql.Split`) is the one place that knows which is which.

**2. Chunk files are the source of truth; SQLite holds only the stream catalog.** The plan lists a
`log_chunks` table. It is not built: at runtime a search reads a chunk's blocks from memory (the
registry rebuilt at start from each chunk's footer, or by walking its blocks), and a table that is
rebuilt from the files at every start and read by nothing would be a second copy of the truth with
no job. SQLite keeps what cannot be rebuilt cheaply, which stream has which id.

**3. Every entry carries a WAL sequence number, stored in the block.** `(ts, seq)` is a total
order over every log, which a pagination cursor needs (timestamps tie constantly), and it is how
recovery tells a sealed entry from one only in the WAL: the entry is dropped if its sequence number
is at or below the largest in its stream's chunk for its day.

**4. A seal writes one block per day, and recovery compares per `(stream, day)`.** A chunk is a
day, and retention deletes whole days. A block that mixed days would be filed under its oldest
entry's day and deleted with it, taking fresh logs along (found by a test: one late log from last
week in a head of fresh ones). With one block per day a crash between two blocks of one seal
leaves one day sealed and the other not, so the "already sealed" test cannot be a single
per-stream maximum; per `(stream, day)` it is exact, because a day's entries are sealed together
and later ones have higher sequence numbers.

The write path is: WAL record, fsync, acknowledge; later each block is written and fsynced, then
published, and only then may WAL segments before the oldest unsealed entry be deleted.

## Alternatives considered

| Option | Why not |
|---|---|
| A `log_chunks` SQL table queried at runtime | It duplicates what the files say and must be reconciled with them after every crash; see 2. |
| One block per seal regardless of day | Retention loses fresh logs filed under an expired day; see 4. |
| Recovery by a per-stream maximum sequence number | A crash between two day-blocks of one seal drops the unsealed day's entries; see 4. |
| Deduplicate replayed entries by content | Two identical lines with the same timestamp are legitimate and would collapse. |
| Index every token (Elasticsearch-style) | The point of the milestone is the cheaper trade; per-block bloom filters (v2) are the middle path. |

## Consequences

- A long-lived stream with a late-arriving old log makes extra small blocks, one per day touched.
- Recovery opens every chunk's footer at start: cost grows with chunks kept, bounded by retention.
- The stream catalog is the one durable thing outside the chunk and WAL files; losing it orphans
  chunk files (they are skipped with a warning, not deleted).
- Sealed-entry detection depends on sequence numbers being assigned in append order and never
  reused. They survive restarts because recovery sets the counter from the WAL and every chunk.
