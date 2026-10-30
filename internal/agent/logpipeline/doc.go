// Package logpipeline turns a line of text a process wrote into a
// [wire.Log]: the agent's half of the log path, run before anything leaves the
// machine.
//
// # The mental model
//
// A log line is unstructured until something gives it structure, and the work
// is cheapest at the source: the agent knows which file or container the line
// came from, so it can pick the right parser, while the store only sees bytes.
// Each line passes through five stages, in this order and for these reasons:
//
//	exclude   drop what nobody wants (health checks) before spending anything on it
//	limit     a token bucket per source, so a runaway logger cannot flood intake
//	parse     JSON, or a named grok pattern, or (Rails) a request grouped by its id
//	remap     message/level/timestamp/trace ids into the log's own fields
//	redact    last, over everything, so no earlier stage can reintroduce a secret
//
// Redaction is last because remapping and grok move values around; running it
// at the end means it sees the final message and every attribute, whichever
// stage produced them.
//
// # Decisions worth knowing
//
//   - Parsers are tried in order and the first match wins, so a specific
//     pattern (uvicorn's access line) must precede the general one that would
//     also match it. A line no parser recognizes is kept as plain text, never
//     dropped: losing a line because its format was unexpected is the worst
//     way to fail.
//   - Level comes from the line. The stream a line arrived on (stderr) is a
//     fallback for lines nothing recognized, because uvicorn writes INFO to
//     stderr and treating the stream as the level would mark every request an
//     error.
//   - A numeric `status` is an HTTP status code, not a severity, and moves to
//     `status_code`; the log's own status is the severity.
//   - Trace ids: a plain trace_id is hexadecimal and Datadog's dd.trace_id is
//     decimal, and a string of digits is valid as both, so the key decides.
//   - Rails prints one request as several lines, interleaved with other
//     requests'. The usual multiline rule (join a line to the one before) is
//     wrong there; lines are correlated by the request id and emitted as one
//     event when "Completed" arrives, or marked incomplete after a timeout.
//     Both the number of open requests and one request's buffered lines are
//     bounded.
//   - Grok patterns are Go regular expressions (RE2). They cannot backtrack,
//     so a hostile line cannot make the agent spin.
//   - Redaction errs toward over-matching: hiding a harmless value costs a
//     little readability, leaking a secret cannot be undone. The default rules
//     know nothing about any particular app; per-app rules are configuration.
//
// What production agents differ in: Vector and Fluent Bit run pipelines as a
// configurable graph of transforms (VRL, Lua) rather than a fixed sequence,
// and Datadog's agent does the equivalent of this package's built-ins with
// "pipelines" server-side. Doing it in the agent here keeps secrets from
// leaving the host at all.
package logpipeline
