// Package server assembles ozyd: it owns the HTTP listener and wires the
// components behind it — intake (/v1/*, from M1), the query API (/api/v1/*),
// the embedded web UI, health and self-metrics.
//
// ozyd is a modular monolith (ADR-0004): one process, with hard seams
// between intake and the stores so M7 can put a queue between them without
// rewriting either side. This package is the only place those components are
// wired together; each of them knows nothing about the others.
//
// In M0 it serves only GET /healthz, GET /debug/vars and (when a UI is
// supplied) the web UI at /.
package server
