// Package sampler decides which received chunks the agent forwards to ozyd.
//
// Statistics are not affected by any of this: the concentrator has already
// counted every span. Sampling only decides which traces are kept as examples,
// and the goal is that the examples are the ones worth looking at.
//
// A chunk is kept if any sampler says so:
//
//   - priority: the SDK's head-sampling decision (`_sampling_priority` >= 1 on
//     any span). Made once at the trace root from the trace id, so every service
//     in the trace agrees. -1 (user drop) is a hard no that nothing overrides.
//   - error: any span with error=1, through a token bucket (10 traces/s), so a
//     failing dependency cannot turn "keep errors" into "keep everything".
//   - rare: the first trace seen for a (service, name, resource) in five minutes,
//     through a token bucket (5/s), so a route that gets one request an hour is
//     never lost to a 1% head rate.
//
// The agent also feeds back a per-service head rate: target traces/s divided by
// the traces/s actually observed for that service, capped at 1. A service doing
// 1000 traces/s is told 0.05 and keeps about 50; a quiet one keeps everything.
// The SDK applies it to the next trace it starts, which is why the keep decision
// stays a pure function of (trace id, rate) rather than a coin flip: a trace that
// crosses services still gets one answer.
//
// A trace kept by the error or rare sampler after the head sampler dropped it is
// partial: only the chunks this agent saw while the decision was "keep". That is
// the honest trade of tail-ish sampling at the agent, and the trace view says so
// by showing orphans under a "missing parent" node instead of hiding them.
package sampler
