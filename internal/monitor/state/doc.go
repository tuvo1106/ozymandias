// Package state is the per-group alert state machine — OK, WARN, ALERT,
// NO_DATA — with recovery thresholds (hysteresis) so a value hovering at the
// threshold does not flap.
//
// # One pure function
//
// Everything is [Step]: previous state plus one observation plus the
// monitor's levels gives the next state and whether a notification is owed.
// It reads no clock and touches no store. That is a design choice with three
// consequences the rest of internal/monitor depends on:
//
//   - Replaying a value sequence replays the events exactly, so a backtest in
//     the editor (a simulated clock over the past day) and the live evaluator
//     draw the same markers, because they run the same function.
//   - A restart is invisible. The scheduler persists [Group] and reloads it;
//     Step cannot tell a reloaded state from one it produced a second ago, so
//     there is no "first evaluation after startup" case to get wrong — and no
//     duplicate notification for an alert that was already announced.
//   - The property tests can quantify over every value sequence instead of a
//     handful of scenarios.
//
// # Hysteresis
//
// Entering ALERT and leaving it use different levels. A queue-depth monitor
// alerting above 20 with critical_recovery 15 enters at 21 and leaves at 15;
// anything in between keeps the current status. Without the band, a value
// sampled at 19, 21, 19, 21 sends a page and a recovery every thirty seconds,
// and a monitor that cries wolf is turned off. Prometheus has no recovery
// thresholds and gets the same effect from `for:` (min_consecutive_failures
// here) plus `keep_firing_for`; Datadog's are the model followed.
//
// # No data is not zero
//
// A group with no value this evaluation is not a group whose value is 0. It
// keeps its status until it has been silent for no_data_timeframe, measured
// from the newest point ever seen against the end of the evaluated window,
// and only then becomes NO_DATA — if the monitor asks for that. A group that
// never had data never becomes NO_DATA.
package state
