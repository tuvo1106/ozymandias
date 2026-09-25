// Package intake is ozyd's front door for telemetry: the /v1/*
// endpoints that decode and validate agent payloads
// (docs/wire-protocol.md §C–F) and hand accepted items to the stores —
// directly until M7, through a durable queue after.
//
// Its contract with senders is about blame. A body that can't be read is
// the sender's fault (400, don't retry). A series that is invalid, or whose
// type conflicts with what the metric was first seen as, is rejected on its
// own and reported in a 202 — the rest of the payload is kept. A store that
// is unavailable is ozyd's fault (503), so the agent retries the whole
// payload, which is safe because a repeated sample overwrites itself.
package intake
