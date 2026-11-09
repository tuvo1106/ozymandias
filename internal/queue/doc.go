// Package queue is a durable, single-node, append-only log of records with
// committed consumer offsets: a small Kafka, built to sit between ozyd's
// intake and its stores.
//
// # Why a queue at all
//
// Before M7, an intake request was answered only after the store had appended
// its contents: a slow compaction, a full disk or a busy Pebble made every
// sender wait, and a crash between "decoded" and "appended" could lose data
// that had already been answered 2xx. Production pipelines put a durable log
// in the middle (Kafka in front of almost every serious metrics and logging
// backend) so that the only thing intake has to do before saying yes is make
// the bytes durable once. Everything after that — indexing, compression,
// compaction — is the store's problem, done at the store's pace, and is
// replayed from the log if it fails.
//
// # The mental model
//
// A [Topic] is a sequence of records numbered 0, 1, 2, … with no gaps. A
// producer appends a record and is told its offset only once the record is
// fsynced: the ack follows durability, never precedes it.
//
// A [Consumer] has two positions. Its *cursor* is how far it has read, and
// lives only in memory. Its *committed offset* is how far it has finished
// processing, and lives in a small fsynced file. After a crash the cursor is
// gone and the consumer resumes at the committed offset, so anything it had
// read and processed but not yet committed is delivered again. That is
// at-least-once delivery, and it is the reason every store behind this queue
// must apply a record idempotently: applying it twice has to leave the same
// state as applying it once. (The alternative, committing before processing,
// is at-most-once: a crash in between loses the record silently.)
//
// # Group commit
//
// An fsync costs milliseconds on a laptop SSD and far more on a cloud disk.
// Syncing once per record would cap a topic at a few hundred records a second.
// So producers append and then wait; one goroutine per topic syncs whatever
// has accumulated, either SyncInterval after the first unsynced append or as
// soon as SyncBytes have piled up, and releases every waiter at once. One
// fsync covers many acks. The interval is a latency floor for an idle topic
// and is the knob for the latency/throughput trade (docs/notes/M7.md has the
// sweep).
//
// Consumers read only below the *synced* end, never the written end, so a
// record is never processed before it is durable: otherwise a crash could
// leave a store holding a record that the queue then forgets, and the
// producer — who was never acked — would send it again under a new offset.
//
// # Retention and the cap
//
// A segment is deleted once every offset in it is at or below every
// consumer's committed offset. A topic that reaches MaxBytes refuses new
// records with [ErrFull] instead of deleting unconsumed ones, because a record
// in the queue has already been acknowledged and dropping it would be exactly
// the silent loss the queue exists to prevent. The caller (intake) turns
// pressure into 429s long before the cap (M7 §2).
//
// # On disk
//
// A topic directory holds WAL segments ([internal/tsdb/wal] framing, reused
// as is) and a consumers/ directory with one offset file per consumer.
// [docs/formats/queue-segment.md] specifies the bytes.
//
// [docs/formats/queue-segment.md]: https://github.com/tuvo1106/ozymandias/blob/main/docs/formats/queue-segment.md
package queue
