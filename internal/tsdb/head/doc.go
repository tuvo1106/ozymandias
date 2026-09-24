// Package head is the TSDB's in-memory window: every sample that has arrived
// but has not yet been written to an immutable block, plus the series identity
// and inverted index over them.
//
// # The mental model
//
// A time series database is an LSM tree with the time dimension doing the
// sorting for it. Writes land here, in memory, appended to compressed chunks.
// Periodically the window is cut: everything older than a boundary is written
// out as a block and dropped from memory. The head is therefore the only
// mutable part of the database — blocks are never edited — and that single
// fact explains most of the rules below.
//
// # Why samples get rejected
//
// Chunks are append-only bitstreams ([chunkenc]): a sample can be added to the
// end and nowhere else. So a sample older than the newest one in its series
// cannot be stored without re-encoding the chunk, and one older than
// MinValidTime cannot be stored at all, because its time range already lives
// in a block. Both are refused and counted ([ErrOutOfOrder], [ErrOutOfBounds]).
//
// The one exception is a repeat of the newest sample with the same value. That
// is what an at-least-once agent retry looks like, it carries no new
// information, and accepting it as a no-op is what makes the wire protocol's
// retries safe.
//
// [ErrSeriesLimit] is a different kind of refusal: not "this cannot be stored"
// but "this should not be". One metric with a `user_id` tag can mint series
// without bound, and the cost is paid by every query against the database, not
// just by the metric responsible. The limit is per metric name so that the
// damage stays local to whoever caused it.
//
// # Concurrency
//
// The series map is sharded across 256 stripes, so resolving a series — the
// common case, and the one on every append — does not serialize. Each series
// then has its own lock for its chunks.
//
// Commit is the exception, and deliberately so. When a WAL is configured, the
// log write and the in-memory apply happen under one lock, which makes WAL
// order and apply order the same order. [Replay] walks the log applying the
// same rules, so it reconstructs exactly the samples that were visible. Let
// the two orders diverge and two appends racing on one series can be logged in
// one order and applied in the other — the head accepts a sample that replay
// then rejects, and a restart silently changes the data. A test pins this.
//
// # Chunk cutting
//
// A chunk is cut at [chunkenc.MaxSamplesPerChunk], and also whenever a sample
// crosses a block boundary. The second rule is what lets a block cut be a pure
// move: no chunk ever spans two blocks, so none has to be split and re-encoded.
//
// # How real systems differ
//
// Prometheus's head is the direct ancestor of this one, down to the striped
// series map and the WAL record types. It goes further in two directions this
// does not: memory-mapped head chunks, so a full head is not a full heap, and
// (since 2.39) an out-of-order write path with its own chunk range, which
// turns [ErrOutOfOrder] from a rejection into a slower accept path. Agent-side
// aggregation sidesteps the question entirely — bucketing into 10s windows
// before anything reaches storage makes late points the agent's problem, not
// the TSDB's — and that is the path this project takes.
package head
