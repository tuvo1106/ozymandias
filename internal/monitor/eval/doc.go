// Package eval evaluates one monitor query over its window (offset by the
// evaluation delay) and reduces each group's points to the single value
// compared against thresholds.
//
// # Why the window ends in the past
//
//	        window (last_5m)            delay
//	  ├──────────────────────────────┤├────────┤
//	──┴──────────────────────────────┴┴────────┴──▶ time
//	from                             to       now
//
// An agent aggregates for ten seconds, flushes, and the forwarder may retry;
// a bucket at now−5s is usually still missing points that will arrive. A
// monitor that reads it sees a dip that is not there — a count monitor alerts
// on "requests dropped to zero" every evaluation. So the window ends
// evaluation_delay_s before now (default 30s), aligned down to the step. The
// cost is that every alert is at least that late, which is the right trade:
// a late page is an annoyance, a false one teaches people to ignore pages.
// See docs/adr/0047-monitors-evaluate-behind-now.md.
//
// # Fetch, then reduce
//
// Evaluation is two functions. [Evaluator.Fetch] reads every group's buckets
// on one grid; [Reduce] folds a slice of that grid into one value per group.
// A live evaluation fetches its own window and reduces it. A backtest fetches
// a day once and reduces a sliding window at every simulated tick. Because
// both run the same Reduce over the same bucket grid, the backtest's markers
// are what the live evaluator would have done, not an approximation of it.
//
// # Absent is not zero (except when it is)
//
// A metric group with no point in the window is absent: the state machine
// decides whether that is NO_DATA. A log count is different — no matching line
// is a count of zero — so log monitors fill zeros and are never absent.
package eval
