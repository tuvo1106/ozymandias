// Package tracestore stores spans in Pebble with keys designed for the
// questions APM asks: fetch a whole trace by id, search entry spans by
// service, resource, duration and error, and derive the service map.
//
// The mental model is one primary table and a few narrow indexes. The primary
// table holds every span under (trace id, span id), so a trace is one prefix
// scan. The indexes cover only entry spans (the first span a service sees of a
// request), because a request is the unit APM lists and an entry span is one per
// request per service: the indexes are a small fraction of the spans they point
// at. Key layout, with the reason for each choice, is in docs/formats/tracestore-keys.md.
//
// Three decisions shape the rest:
//
//   - Time is stored inverted (^start), so a forward scan is newest first and
//     "the latest 50" is a seek and 50 Next calls, not a sort.
//   - The service map is a counter per (hour, env, parent, child) maintained with
//     a Pebble merge operator as spans arrive, not a join over spans at read time.
//     A child whose parent has not arrived yet waits in a small bounded map; one
//     whose parent never arrives is counted as unresolved and forgotten.
//   - Retention is by trace, found through a first-seen index, because the
//     indexes are keyed by service and time and cannot be range-deleted by age
//     without also deleting traces that merely started late.
//
// The store does not sample: it keeps what the agent forwards, and the RED
// statistics come from metrics, which the agent computed before sampling.
package tracestore
