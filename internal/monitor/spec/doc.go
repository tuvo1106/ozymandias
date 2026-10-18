// Package spec is what a monitor *is*: the definition, the query grammar and
// the message template, and the rules that decide whether one is worth saving.
//
// Like internal/dashboard, it neither stores nor runs anything. The scheduler
// (internal/monitor), the evaluator (internal/monitor/eval) and the API all
// consume a [Compiled] monitor, so a definition POSTed by the UI and one read
// from deploy/monitors/ pass exactly the same checks.
//
// # A monitor query is a comparison wrapped around a query
//
//	avg(last_5m):avg:arq.queue.depth{service:api} by {queue} > 20
//	└───┬────┘   └───────────────┬────────────────────────┘ └─┬─┘
//	    │                        │                            └ op and threshold
//	    │                        └ ordinary metricql: what a dashboard draws
//	    └ reduce each group's points over the window to one number
//
// The inner query is parsed by the metricql (or logql) parser, so a monitor
// accepts what a chart does and fails with the same messages. The comparison
// is read from the *right*, because it is the one part guaranteed to end the
// string; a tag value can contain '>' but cannot be followed by a bare number
// at the end.
//
// # Why validation is strict
//
// A monitor is code that runs unattended for months and is consulted at the
// worst moment. Every mistake this package can see — a misspelt option, a
// recovery level on the wrong side, a template field that does not exist — is
// a monitor that saves cleanly and then does not page. So all of them are
// refused at save, with the field named, rather than tolerated.
package spec
