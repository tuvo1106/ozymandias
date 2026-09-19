// Package eval evaluates one monitor query over its window (offset by the
// evaluation delay) and reduces each group's points to the single value
// compared against thresholds.
//
// Status: arrives in M6 (docs/plan/M6-monitors.md); this file marks its place in
// the architecture until then.
package eval
