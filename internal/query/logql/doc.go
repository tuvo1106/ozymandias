// Package logql parses the log search syntax (service:api status:error
// "timeout" @duration:>500), splits each query into stream matchers answered
// by the index and a residual filter answered by scanning, and evaluates
// filters over logs.
//
// # The mental model
//
// A log search is a boolean expression over three kinds of term:
//
//	service:api status:error     reserved fields every log has (a [Label])
//	@duration:>500 @user.id:42   attributes of the structured log (an [Attr])
//	"timeout" err*               free text in the message or any string attribute (a [Text])
//
// joined by juxtaposition or AND (tighter), OR, and negated by `-` or NOT.
//
// The point of the package is the split. Loki's insight, which this store
// follows, is that most of a log search names the stream (which service,
// which status) and only some of it needs to read text. [Split] lifts the
// stream-naming conjuncts out as [Matcher]s, which the index answers without
// touching a log, and leaves the rest as a [Node] to check against the logs of
// the streams that survive. [Compile] turns any node into a [Filter], the one
// definition of "this log matches" shared by the store's scan and the live
// tail, so a query cannot mean one thing on disk and another on screen.
//
// # Deliberate choices
//
//   - Unknown `key:value` is an error, not free text. `error:timeout` is
//     nearly always an attribute typed without its `@`, and silently searching
//     for the string would find nothing and say nothing.
//   - Only a top-level OR is distributed into index branches; a nested OR is
//     scanned. Distribution multiplies branches per nested OR; the filter
//     already evaluates labels correctly.
//   - The parser is scannerless: whether `-`, `>` or `a:` mean something
//     depends on where they stand, which a separate lexer would have to
//     guess.
//   - A missing label is the empty string, and `*` alone means "has a value".
//     The index and the scan share one function for this ([MatchLabel],
//     [LabelValue]) so they cannot disagree.
//
// Printing a parsed query gives text that parses back to the same tree
// (property- and fuzz-tested), which is what lets an API echo a normalized
// query and a saved view be compared as text.
package logql
