# ADR-0044: Head sampling in the SDK; error, rare and statistics in the agent

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

Keeping every trace is unaffordable at scale; keeping a random few loses exactly the traces that
matter (the failing one, the slow one, the route nobody calls). And request counts computed from
what was kept are samples, wrong by the sample rate.

## Decision

1. **The SDK decides once, at the trace root**, with `(low64(trace_id) * K mod 2^64) < rate * 2^64`,
   a pure function of the trace id, so every service in a trace agrees without coordinating, in
   any language (shared vectors check Python, Node and Go). Downstream services inherit the
   upstream decision. The rate comes from the agent's `rate_by_service` response: target traces/s
   over observed, capped at 1.
2. **Unsampled traces are still sent to the agent** (priority 0), which needs every span to count.
3. **The agent computes request, error and latency statistics from every span, before sampling**,
   by feeding the ordinary aggregator, and only then asks its samplers: priority (the SDK's
   decision), error (any error span, token bucket 10/s) and rare (first of a (service, name,
   resource) per five minutes, 5/s). A chunk is kept if any says keep; a user-drop priority of -1
   is final.

## Alternatives considered

| Option | Why not |
|---|---|
| Tail sampling in ozyd (keep after seeing the whole trace) | Needs every span shipped over the network and buffered until a trace is complete; the agent cannot know completeness across processes. The agent-side error and rare samplers get most of the value at a chunk's granularity. |
| SDK-only sampling | Statistics become samples, and the SDK cannot know a route is rare. |
| Compute statistics in ozyd from stored spans | They would be exactly as sampled as the store. |

## Consequences

A trace kept by the error or rare sampler after the head sampler dropped it may be partial (only
the chunks this agent saw). The trace view shows missing parents rather than hiding them. The SDKs
send more spans than are stored, which is the price of exact statistics (measured in the notes).
