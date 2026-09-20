// Package forwarder ships the agent's series to ozyd: split into
// payloads the intake accepts, gzip, POST, and retry what failed for a
// retryable reason with jittered exponential backoff, honoring Retry-After.
//
// Memory is bounded: payloads waiting for delivery are capped in bytes, and
// when a new one doesn't fit the oldest are dropped (and counted). After an
// outage the recent past is what matters, and an agent that grows until the
// host kills it loses everything instead of the oldest few minutes.
//
// The caller never waits for the network — Submit only queues — so a slow
// or dead ozyd can't back up the aggregator or, through it, the apps.
// M7 adds spilling to disk to survive longer outages.
package forwarder
