// Package wal is a segmented, checksummed write-ahead log: every append is
// written here before it is acknowledged, and replayed on startup to rebuild
// in-memory state after a crash. Reused by the log store (M4) and the durable
// queue (M7).
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package wal
