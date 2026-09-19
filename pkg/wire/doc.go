// Package wire holds the Go types, encoders and validators for every payload
// in docs/wire-protocol.md. It is the only package shared by the agent and
// ozyd, and its golden files are shared with both SDKs so three
// implementations cannot drift.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package wire
