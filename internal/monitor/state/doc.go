// Package state is the per-group alert state machine — OK, WARN, ALERT,
// NO_DATA — with recovery thresholds (hysteresis) so a value hovering at the
// threshold does not flap.
//
// Status: arrives in M6 (docs/plan/M6-monitors.md); this file marks its place in
// the architecture until then.
package state
