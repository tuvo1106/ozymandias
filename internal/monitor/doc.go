// Package monitor runs monitors: it schedules evaluations, owns their
// persisted per-group state and routes transitions to notifiers.
//
// The pieces, each its own package so each can be tested alone:
//
//	spec    what a monitor is: definition, query grammar, message template
//	eval    run the query over the window, reduce each group to one value
//	state   the per-group state machine, as one pure function
//	notify  notifiers, and the dispatcher that delivers from the outbox
//
// This package is the loop that joins them: [Scheduler].
//
// # One evaluation
//
//	due ──▶ eval.Evaluate ──▶ advance (state.Step per group) ──▶ meta.Commit ──▶ Enqueue
//	        query + reduce     pure: nothing changed yet          one tx:         delivery,
//	                                                               states,         off this path
//	                                                               events,
//	                                                               outbox
//
// Nothing is adopted in memory until the commit succeeds. A database failure
// mid-transition therefore leaves the scheduler believing exactly what the
// database believes, and the same transition is recomputed — and announced —
// on the next evaluation. A restart reloads the same rows, so it is
// indistinguishable from no restart: an ALERT already announced is not
// announced again, and a renotify timer continues from last_notified_ts.
//
// # Scheduling
//
// Each monitor runs every evaluation_interval_s at a phase fixed by a hash of
// its uid ([Offset]), so monitors spread over the interval rather than all
// querying at once. A bounded pool runs evaluations; a monitor whose previous
// evaluation is still running when it is due again skips that tick and counts
// ozy.monitor.eval_skipped, rather than queueing behind itself.
//
// # Watching the watcher
//
// If monitors exist and none has completed an evaluation for a while, alerting
// is down and every monitor's silence is meaningless. [Scheduler.Health] says
// so, the scheduler logs it loudly once, the UI shows a banner, and
// ozy.monitor.last_eval_age_seconds is there for anything outside ozymandias
// to watch.
//
// # Backtests use the same code
//
// [Scheduler.Backtest] replays a monitor over the past on a simulated clock
// through the same window, reduction and [advance] the live path uses, so the
// editor's "would have alerted" markers are what the scheduler then does.
package monitor
