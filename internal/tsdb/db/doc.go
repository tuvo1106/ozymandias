// Package db is the real time series database: it ties the write-ahead log,
// the in-memory head and the immutable blocks into one [tsdb.MetricStore].
//
// # The mental model
//
// Every durable store has to answer the same question — how does a write
// become both safe and fast to read — and they nearly all answer it by
// splitting the job three ways:
//
//   - The **log** makes a write durable immediately. It is append-only, so it
//     is fast, and it is unordered and unindexed, so it is useless for reading.
//   - The **head** makes a write queryable immediately. It is indexed and
//     compressed in memory, and it is gone the moment the process dies.
//   - **Blocks** make a write both, and are written only in large batches,
//     because that is the only way to produce a file that is compact, indexed
//     and immutable.
//
// A sample lives in the log and the head at first, then in a block. Where it
// is at any moment is nobody's business but this package's: [DB.Select] merges
// whichever sources overlap the query window, and a test asserts a query's
// answer does not change when its data is cut to disk.
//
// # The order of operations, and why
//
// On startup: clear the leftovers of interrupted writes and deletes, open the
// blocks, then replay the log. That order means a half-written block is never
// read, and a sample a block already holds is refused by the head's own bounds
// check rather than duplicated.
//
// On a block cut: write the block and make it visible, then let the head
// forget what the block now holds, then truncate the log. A crash between any
// two steps leaves the data in *both* places, never neither — and replay
// resolves the overlap. The reverse order would have a window where it is in
// neither.
//
// On close: flush the log and stop. Deliberately no block cut. The log already
// holds everything the head does, replay is how it comes back, and doing the
// riskiest operation in the codebase during shutdown is how stores lose data.
//
// # What it cannot do
//
// Samples are append-only (ADR-0011), so backfill and late batches are
// rejected rather than stored, and retention deletes whole blocks rather than
// ranges of samples — which is why the oldest data can outlive its window by
// up to one block range.
//
// # How real systems differ
//
// Prometheus's `tsdb.DB` is the direct model, including the 1.5-range cut
// threshold and the WAL checkpoint. It adds what this does not have yet:
// leveled compaction, an out-of-order write path, and memory-mapped head
// chunks. Commercial backends split the job across processes instead — intake,
// storage and query are separate services — which buys scale and costs exactly
// the simplicity that makes this package readable in one sitting.
package db
