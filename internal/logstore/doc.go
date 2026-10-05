// Package logstore is a Loki-style log store: logs grouped into low-
// cardinality streams, buffered in memory, sealed into compressed chunk files,
// found by a label index followed by a scan.
//
// # The mental model
//
// Elasticsearch indexes every word of every log, which makes search fast and
// storage large. Loki's bet, which this package makes, is the opposite: index
// only the labels that name a stream (service, source, host, env, status), keep
// the log text itself compressed and unindexed, and answer a search by
// selecting streams with the index and then reading their blocks. Most
// searches name a service and a status before they name a word, so the scan is
// over a small slice of the data. The cost is that a free-text search with
// nothing else to narrow it reads everything in range; per-block bloom filters
// (the format's v2) exist to skip blocks that cannot contain a word.
//
//	Append ──► WAL (fsync) ──► head block (per stream, in memory)
//	                                │ sealed at 256 KiB or 5 minutes
//	                                ▼
//	                  chunk file  <day>/<stream>.chunk : zstd blocks + footer
//	Search ──► streams (labels) ──► blocks overlapping the range ──► filter
//
// # What makes it correct
//
//   - Durability is ordering. A batch is acknowledged only after its WAL
//     record is fsynced. A block is fsynced before it is published, and WAL
//     segments are deleted only after that. A crash at any point leaves each
//     log in the WAL, in a block, or both, and recovery drops the WAL copy of
//     an entry whose sequence number a block already holds (per stream and
//     day: see ADR-0039 for why per day).
//   - Chunk files are the source of truth. SQLite holds only the stream
//     catalog; everything else is rebuilt at start from the chunk footers and
//     the WAL.
//   - A search sees every log exactly once. A seal publishes a block and
//     empties the head in one step under one lock, and a search snapshots
//     under that lock, so a log is in the head or in a block, never both and
//     never neither. Published blocks are immutable, so the reads that follow
//     take no lock.
//   - Order is total. Every entry has a WAL sequence number, so (timestamp,
//     sequence) orders all logs, and a pagination cursor is exactly that pair:
//     paging neither repeats nor skips a log while ingest continues.
//
// # Reading
//
// [Store.Search] decodes blocks lazily: it merges the streams' blocks in
// order of their near timestamp edge, decoding a block only when it could hold
// the next log, so a newest-first search for a page reads a few blocks of the
// newest data, not the range. A per-query scan budget makes an unselective
// search stop with a correct prefix and a cursor instead of running away.
// [Store.Aggregate] and [Store.Facets] count without ordering and so skip the
// merge.
//
// The on-disk format is docs/formats/log-chunk.md; the query syntax and its
// split into index and scan is package logql.
package logstore
