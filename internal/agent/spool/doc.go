// Package spool is the agent's disk buffer: a FIFO of gzip'd intake payloads
// that outlives the process and the memory cap.
//
// # Why it exists
//
// The forwarder keeps payloads that failed to send in memory, up to a cap, and
// drops the oldest beyond it. That survives a restart of ozyd for a few
// minutes, and an agent restart not at all. A spool takes what memory cannot
// hold, and what is still queued when the agent stops, and keeps it on disk
// until ozyd answers. The agent's memory stays bounded however long ozyd is
// down; the disk is bounded too, and only then is the oldest data dropped.
//
// # The mental model
//
// One spool per intake endpoint (series, sketches, spans). Records are
// appended to the log segments of internal/tsdb/wal (length, type, CRC32C: a
// torn or flipped record is detected, never trusted) and consumed strictly
// from the front. There is one consumer, the forwarder's send loop, so there is
// no consumer registry or offset table as in internal/queue: the spool keeps
// one cursor, the end offset of the last payload delivered.
//
// # Two decisions worth knowing
//
// Whole segments are the unit of deletion. A segment is removed when its last
// payload is delivered, and the oldest segment is removed when the spool is
// over its cap. So "drop the oldest" is one unlink, not a rewrite, at the cost
// of dropping up to one segment's worth more than strictly needed.
//
// The cursor is persisted without an fsync. A crash of the process (kill -9)
// loses nothing it wrote; a crash of the machine can lose the newest cursor,
// and the spool then re-sends payloads it had already delivered. That is safe
// because every intake path is idempotent for a repeated payload (the TSDB
// no-ops a repeated sample, sketches and spans overwrite by key), and it is
// cheaper than an fsync per delivery. The payloads themselves are fsynced when
// they are written, because that write is the one that must survive.
//
// # Damage
//
// A record that fails its checksum ends its segment: everything after it in
// that segment is skipped, counted as dropped, and the spool carries on with
// the next segment. A spool that stopped at the first bad byte would hold every
// later, healthy payload hostage.
//
// Format: docs/formats/agent-buffer.md.
package spool
