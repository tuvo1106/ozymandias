// Package intake is ozyd's front door for telemetry: the /v1/* endpoints
// that decode and validate agent payloads (docs/wire-protocol.md §C–F) and
// hand accepted items to the stores — directly until M7, through a durable
// queue after.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package intake
