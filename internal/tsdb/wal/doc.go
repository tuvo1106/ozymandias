// Package wal is a segmented write-ahead log: an append-only record stream
// that survives a crash, and the thing that lets everything above it keep its
// working set in memory.
//
// # The mental model
//
// The head holds recent samples in RAM because that is the only way to make
// ingestion fast. RAM does not survive `kill -9`. The WAL is the bargain
// between those two facts: before a sample is acknowledged, its bytes are in
// a file and (by policy) fsynced, so a crash costs at most the un-synced
// tail. On restart the log is replayed to rebuild exactly the state that was
// acknowledged, and nothing that wasn't.
//
// That makes durability a property of *ordering*, not of writing more: write
// the log entry, sync it, then acknowledge. A store that acknowledges first
// is fast and lying.
//
// # Segments, and why truncation is the interesting part
//
// The log is a directory of fixed-size segments (`00000000.wal`), which makes
// deletion possible: a whole file can be unlinked once its contents are
// durable elsewhere (a block on disk). A single growing file could only be
// truncated from the front, which no filesystem does well.
//
// Deleting a segment can orphan information, though — sample records refer to
// a series by a short id defined by a *series record* that may live in an
// older segment. So truncation writes a checkpoint first: the records still
// needed by surviving segments are copied forward into `checkpoint.%08d`,
// which the reader replays before the segments. Prometheus does the same.
//
// # Torn tails versus corruption
//
// A crash mid-write leaves a partial record at the end of the last segment.
// That is expected and is not an error: the reader stops there, and the log
// is truncated to the last good record. The same damage in the *middle* of
// the log is different — it means bits rotted or something wrote where it
// should not have — and is reported as an error naming the segment and
// offset, because silently skipping it would lose acknowledged data without
// telling anyone.
//
// # Generic on purpose
//
// A record is a type byte and opaque bytes. This package knows nothing about
// series or samples; the TSDB defines its own record types, and M4's log
// store and M7's queue reuse the same log. [docs/formats/wal.md] specifies the
// bytes.
//
// [docs/formats/wal.md]: https://github.com/tuvo1106/ozymandias/blob/main/docs/formats/wal.md
package wal
