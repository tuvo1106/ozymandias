# ADR-0003: Own wire protocol and SDKs, not OpenTelemetry first

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

Apps need a way to send metrics, logs and traces. OpenTelemetry would make the stock OTel SDKs work immediately, but writing the client side is where you learn what an SDK actually does: buffering, sampling, context propagation, patching libraries.

## Decision

Define an ozymandias wire protocol (docs/wire-protocol.md): extended-StatsD-compatible UDP for metrics, and JSON over HTTP for traces, logs and agent-to-server traffic. Write zero-dependency Python and Node SDKs against it. The protocol, not the SDKs, is the public interface. OTLP is added as a receiver in M8, so the standard can be compared with what was designed.

## Alternatives considered

| Option | Why not |
|---|---|
| OTLP from day one | It skips the SDK internals, which are one of the four areas the project is meant to teach. |
| OTLP never | Most of the world speaks it, and app-ruby (Ruby) has no ozymandias SDK, so an OTLP receiver is how its traces arrive. |

## Consequences

There are two SDKs to maintain. The protocol must stay additive within `/v1`. Anything that can send UDP or HTTP can participate without an SDK (ADR-0008).
