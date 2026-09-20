// Package wire holds the Go types, encoders and validators for the payloads
// in docs/wire-protocol.md, which is normative: when this package and the
// document disagree, the document wins and this package has a bug.
//
// It is the only package shared by the agent and ozyd, and its golden
// files (testdata/) are loaded by both SDKs' test suites too, so the three
// implementations of the protocol cannot drift apart unnoticed.
//
// The protocol, not any SDK, is ozymandias's public interface: anything that
// can send a UDP datagram or an HTTP POST can report metrics. That is why
// validation lives here, at the boundary, and is strict about structure but
// forgiving about cosmetics — NormalizeMetricName and NormalizeTag repair
// what can be repaired (a '-' in a name becomes '_') and reject only what
// can't, so a sloppy client loses a character, not its data.
//
// What is here, by hop:
//   - names.go: metric-name and tag rules shared by every hop.
//   - series.go: hop C, agent → ozyd POST /v1/series, and the response
//     shapes every intake endpoint shares.
//
// The statsd datagram (hop A) is parsed in internal/agent/statsd, next to
// the UDP server whose hot path it is; its shared goldens live here in
// testdata/statsd/.
package wire
