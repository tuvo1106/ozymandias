// Package logpipeline is the ordered chain of processors every log line passes
// through in the agent: parse (JSON or grok-style patterns), remap reserved
// fields, redact secrets, exclude or sample, then batch for forwarding.
//
// Status: arrives in M4 (docs/plan/M4-logs.md); this file marks its place in
// the architecture until then.
package logpipeline
