// Package block writes and reads immutable on-disk TSDB blocks: a time range
// of compressed chunks plus the index that locates them, written atomically so
// a crash can never expose a half-written block.
//
// # The mental model
//
// The head holds recent samples in memory and is the only mutable part of the
// database. Periodically its oldest window is written out here and dropped
// from memory. A block is that window frozen: never edited, never appended to,
// only read or deleted. Compaction does not change blocks either — it writes a
// new one and removes its sources.
//
// Immutability is what pays for everything else. A block needs no locks, can
// be read by any number of goroutines, can be copied with `cp`, and its files
// can be checksummed once at write time and trusted forever after.
//
// # The three files
//
//	<ULID>/
//	  meta.json   what this block is: time range, stats, provenance
//	  chunks.dat  the samples, gorilla-encoded, each record checksummed
//	  index.dat   symbols, series, postings, and a sorted offset table
//
// [ULID] ids sort by creation time, so `ls` is chronological and compaction
// can find adjacent blocks without opening anything.
//
// The index is the interesting file. Two ideas make it small and fast:
//
//   - A *symbol table*. Tag strings repeat across series — one `env`, one
//     `prod`, a handful of route names — so each distinct string is stored
//     once and referenced by ordinal. On a realistic block this is an order of
//     magnitude: measured at 666 bytes of symbols against 12 KB of the same
//     strings written inline (see the test).
//   - A sorted *offset table* from (key, value) to a postings list, so a
//     lookup is a binary search and only the pairs a query mentions are ever
//     decoded. Symbols are sorted, so the table can be searched on ordinals
//     without touching a single string.
//
// Each chunk reference carries its own time range, so a five-minute query
// against a two-hour block reads a handful of chunks rather than all of them.
//
// # Why a block is never half-visible
//
// Everything is written into `<ULID>.tmp/`, fsynced, and only then renamed
// into place; the parent directory is fsynced after the rename. Rename is
// atomic, so a block either appears complete or does not appear. A crash
// leaves a `.tmp` directory, or a directory with no meta.json, and
// [CleanTmp] removes both at startup. Deletion runs the same argument
// backwards: a tombstone file goes down first, so an interrupted delete is
// finished by [CleanCondemned] rather than leaving a block missing files.
//
// Corruption is caught, not guessed at: every chunk record carries a CRC32C
// over its own bytes, and index.dat carries one over the whole file. The
// checksum is why the parser's bounds checks are nearly unreachable from a bit
// flip — and they exist anyway, because a checksum proves the bytes are the
// ones that were written, not that they mean anything.
//
// # How real systems differ
//
// This is Prometheus's block layout with the hard parts removed. Prometheus
// mmaps index and chunks and lets the page cache decide what stays resident;
// this reads the index whole at open and preads chunks, which is simpler,
// measurably slower on large blocks, and identical in format. Prometheus also
// splits chunks.dat into 512 MB segments, keeps a separate tombstones file for
// deleted series, and has a label-value index within the symbol table for
// faster metadata queries. Closed-source backends cannot be read directly, but
// their shape — immutable time-ranged files, compacted in levels, with a
// separate rollup resolution — is visible in their query semantics.
package block
