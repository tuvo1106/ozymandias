// Package tailer follows log files and container stdout reliably: offsets
// checkpointed per file identity, rotation and truncation handled, and offsets
// committed only after the batch holding those lines is acknowledged (at-
// least-once delivery).
//
// Status: arrives in M4 (docs/plan/M4-logs.md); this file marks its place in
// the architecture until then.
package tailer
