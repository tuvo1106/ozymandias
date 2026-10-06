# ADR-0041: Own propagation headers now, W3C `traceparent` in M8

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

A trace crosses processes through a carrier: HTTP headers, or the kwargs of a queued job. Every
SDK and every integration has to read and write it, so it is a public, versioned surface
(extensibility.md). The industry answer is W3C Trace Context (`traceparent`/`tracestate`), which
also lets OpenTelemetry-instrumented services join a trace.

## Decision

M5 propagates `x-ozy-trace-id`, `x-ozy-parent-id` and `x-ozy-sampling-priority` (and the same three
keys under `_ozymandias` for job kwargs). Ids are already W3C-shaped (128-bit trace, 64-bit span,
lowercase hex), so M8 adds `traceparent` as a second accepted and emitted form with no change to
the span model. A malformed header yields *no context*, never a partial one: the receiver starts a
new trace rather than continuing a corrupt one.

## Alternatives considered

| Option | Why not |
|---|---|
| `traceparent` only, from the start | The sampling priority has no home in it (`tracestate` is vendor-keyed free text), and the point of M5 is to learn what the carrier must say. Building the parser for a format we would then reshape is wasted work. |
| Both from the start | Two code paths in two SDKs before either is exercised; M8 needs OpenTelemetry interop and is where it is tested. |

## Consequences

Services outside the SDKs cannot join a trace until M8. The wire format of a span does not depend
on this choice, so nothing stored changes.
