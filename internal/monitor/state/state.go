package state

import (
	"math"
	"time"

	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// Status is a group's alert state.
type Status string

// The statuses. Their spelling is the API's and the events table's.
const (
	OK     Status = "OK"
	Warn   Status = "WARN"
	Alert  Status = "ALERT"
	NoData Status = "NO_DATA"
)

// Valid reports whether s is one of the four statuses.
func (s Status) Valid() bool { return s == OK || s == Warn || s == Alert || s == NoData }

// Group is everything remembered about one (monitor, group) between
// evaluations. It is the row the monitor_states table persists, and the whole
// input the next step needs besides the new observation: nothing else is
// consulted, which is what makes [Step] replayable.
type Group struct {
	Status Status
	// Since is when Status was entered.
	Since time.Time
	// Breaches counts consecutive evaluations that breached critical, for
	// min_consecutive_failures. Reset by any evaluation that did not.
	Breaches int
	// LastValue is the most recent value; NaN before the first.
	LastValue float64
	// LastEval is the wall time of the last evaluation that saw this group.
	LastEval time.Time
	// LastData is the timestamp of the newest point ever seen for the group.
	// Zero means never — and a group that never had data is never NO_DATA:
	// "the heartbeat stopped" needs a heartbeat to have started.
	LastData time.Time
	// LastNotified is when a notification for this group was last sent (or,
	// precisely, committed for sending). Zero means never. Renotify timers run
	// from it, so they survive a restart.
	LastNotified time.Time
}

// New is the state a group starts in the first time it is seen.
func New(now time.Time) Group {
	return Group{Status: OK, Since: now, LastValue: math.NaN()}
}

// Observation is one evaluation's view of one group.
type Observation struct {
	// Now is the wall time of the evaluation.
	Now time.Time
	// WindowEnd is the end of the window the value was computed over: Now
	// minus the evaluation delay. No-data age is measured against it, since
	// LastData is a data timestamp and comparing it with the wall clock would
	// count the delay as silence.
	WindowEnd time.Time
	// Present is false when the group returned no value this evaluation.
	Present bool
	Value   float64
	// DataTs is the timestamp of the newest point behind Value.
	DataTs time.Time
}

// Config is what [Step] needs of a monitor.
type Config struct {
	Levels          spec.Levels
	MinFailures     int
	NoDataTimeframe time.Duration // 0: no-data notification off
	Renotify        time.Duration // 0: off
	Muted           bool
}

// ConfigOf builds a Config from a compiled monitor.
func ConfigOf(c *spec.Compiled, muted bool) Config {
	return Config{Levels: c.Levels, MinFailures: c.MinFailures, NoDataTimeframe: c.NoDataTimeframe,
		Renotify: c.Renotify, Muted: muted}
}

// Notify says whether a step owes a notification, and why.
type Notify string

// The notification reasons.
const (
	NotifyNone       Notify = ""
	NotifyTransition Notify = "transition"
	NotifyRenotify   Notify = "renotify"
)

// Result is what a step decided.
type Result struct {
	// From and To are the statuses either side of the step; equal when nothing
	// changed.
	From, To Status
	Notify   Notify
}

// Changed reports whether the status moved.
func (r Result) Changed() bool { return r.From != r.To }

// Step advances one group by one observation.
//
// It is a pure function of (previous state, observation, config): no clock,
// no store, no randomness. Everything the scheduler does around it — persist,
// emit an event, send — is driven by the Result, so replaying a value sequence
// replays the events exactly, and a restart that reloads the previous state
// cannot tell it was restarted.
//
// The transitions are the table in docs/plan/M6-monitors.md §2, and
// DESIGN.md#monitor-state-machine draws them.
func Step(prev Group, obs Observation, cfg Config) (Group, Result) {
	next := prev
	next.LastEval = obs.Now
	if !obs.Present {
		next.Status = noData(prev, obs, cfg)
		if next.Status == NoData {
			next.Breaches = 0
		}
	} else {
		next.LastValue = obs.Value
		if obs.DataTs.After(next.LastData) {
			next.LastData = obs.DataTs
		}
		next.Status, next.Breaches = evaluate(prev, obs.Value, cfg)
	}
	res := Result{From: prev.Status, To: next.Status}
	if res.Changed() {
		next.Since = obs.Now
	}
	switch {
	case cfg.Muted:
		// State advances; nobody is told. See docs/operations.md#muting.
	case res.Changed():
		res.Notify = NotifyTransition
	case cfg.Renotify > 0 && (next.Status == Alert || next.Status == NoData) &&
		obs.Now.Sub(prev.LastNotified) >= cfg.Renotify:
		res.Notify = NotifyRenotify
	}
	if res.Notify != NotifyNone {
		next.LastNotified = obs.Now
	}
	return next, res
}

// noData decides the status of a group with no value this evaluation.
func noData(prev Group, obs Observation, cfg Config) Status {
	if cfg.NoDataTimeframe <= 0 || prev.LastData.IsZero() {
		return prev.Status
	}
	if obs.WindowEnd.Sub(prev.LastData) >= cfg.NoDataTimeframe {
		return NoData
	}
	return prev.Status
}

// evaluate decides the status of a group with value v.
//
// The hysteresis is the asymmetry between entering and leaving: ALERT is
// entered by breaching critical, and left only by falling past
// critical_recovery. A value hovering between the two stays where it is,
// whichever side it came from, so it produces no transitions at all — that is
// the acceptance criterion "oscillating inside the band notifies nothing".
func evaluate(prev Group, v float64, cfg Config) (Status, int) {
	lv := cfg.Levels
	from := prev.Status
	if from == NoData {
		// Data came back: evaluate as if fresh. Breaches were reset on entry.
		from = OK
	}
	crit := lv.Breach(v, lv.Critical)
	breaches := 0
	if crit {
		breaches = prev.Breaches + 1
	}
	minFail := max(cfg.MinFailures, 1)
	switch {
	case crit && breaches >= minFail:
		return Alert, breaches
	case from == Alert && lv.Breach(v, lv.CriticalRecovery):
		return Alert, breaches
	case lv.HasWarning && lv.Breach(v, lv.Warning):
		return Warn, breaches
	case lv.HasWarning && (from == Warn || from == Alert) && lv.Breach(v, lv.WarningRecovery):
		return Warn, breaches
	}
	// Reached also by a value breaching critical fewer than min_consecutive_failures
	// times with no warning level: there is nothing to report yet, so OK.
	return OK, breaches
}
