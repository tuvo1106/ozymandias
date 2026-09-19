// Package sampler decides which traces the agent keeps: the SDK's head-
// sampling priority, plus error and rare-resource samplers so the interesting
// traces survive a low sample rate, and the per-service rate feedback returned
// to SDKs.
//
// Status: arrives in M5 (docs/plan/M5-tracing.md); this file marks its place in
// the architecture until then.
package sampler
