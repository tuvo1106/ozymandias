// Package compact merges small adjacent blocks into larger ones.
//
// # Why merge at all
//
// A head cut produces one block per block range, so a database running for a
// week holds eighty-odd of them. Every query that spans a day opens every
// index in that day, and every one of those blocks holds its own copy of the
// same symbol table — the same metric names, the same `env`, the same handful
// of hosts, written out once per block.
//
// Merging fixes both. Three blocks become one index, one symbol table, and —
// because compaction re-encodes rather than copies — chunks packed to their
// 120-sample capacity instead of cut short wherever a block boundary happened
// to fall. That last part is measurable: three blocks of 60 samples hold three
// short chunks, and the merged block holds two full ones.
//
// # Why levels
//
// Blocks merge only with blocks of their own level: three level-0 blocks make
// a level-1, three level-1 make a level-2. The number of files then grows with
// the logarithm of the data rather than linearly, and each byte is rewritten a
// bounded number of times.
//
// Merging across levels instead would mean folding each new small block into
// the large one beside it, rewriting the large one every time — the write
// amplification that leveled compaction exists to avoid. [DefaultMaxBlockRange]
// stops the levels climbing forever: past some width a block stops helping,
// because a short query still pays for its index and retention can only drop
// data one whole block at a time.
//
// # Crash safety
//
// [Run] writes the merged block and renames it into place before it touches a
// single source. A crash in between leaves the data twice — the merged block
// and its sources, overlapping — and never leaves a gap. Startup finishes the
// deletions ([block.CleanCondemned]), and a later compaction that does meet
// overlapping sources collapses the duplicate timestamps rather than failing,
// because a chunk cannot hold the same instant twice.
//
// # What is not here yet
//
// Downsampled rollups (M2 PR 2) will be written as sibling blocks carrying a
// non-zero resolution, at the same time range as their source. The planner
// already refuses to merge across resolutions, so a rollup and the raw data it
// came from can never end up in one block.
//
// # How real systems differ
//
// Prometheus compacts on the same three-block rule and adds what this does
// not: deletion tombstones folded in during the merge, a `--storage.tsdb.
// max-block-duration` derived from the retention window, and vertical
// compaction for the overlapping blocks its out-of-order write path produces.
// Thanos and Cortex run compaction as a separate process against object
// storage, which is the same algorithm with the network where the disk is.
package compact
