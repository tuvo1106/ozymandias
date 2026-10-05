// Package loghub fans accepted logs out to live-tail subscribers.
//
// The intake publishes every log it has durably stored; each subscriber has a
// compiled logql filter and a bounded channel. The rule that shapes the whole
// package is that publishing never waits: the intake is the write path of the
// store, and a browser tab that stopped reading must not slow it, or stop it.
// A subscriber that cannot keep up loses logs instead (counted, and reported
// to it as a dropped notice), which is the right trade for a live view — the
// store has every log, and tail is a window onto the present, not a record.
package loghub
