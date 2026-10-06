// Package tracerecv is the agent's trace intake: POST /v1/traces on the agent's
// HTTP port (:8126), the endpoint the SDKs' tracers flush to.
//
// A request is a list of chunks, each the spans of one trace that one process
// finished together. The handler's job is to be a cheap, safe front door:
//
//  1. take a slot from a small semaphore, or answer 429 — the SDK drops the
//     chunk and moves on, because traces are best-effort and a slow agent must
//     never become a slow app;
//  2. decode and validate each span on its own, refusing the invalid ones and
//     normalizing the rest (pkg/wire), so nothing downstream re-checks;
//  3. hand every chunk to the stats concentrator first, then to the samplers —
//     the order is the point (see package concentrator);
//  4. forward only the kept spans to ozyd, and answer with the head-sampling
//     rate each service should use next.
//
// Nothing here blocks on the network: forwarding is a queue append.
package tracerecv
