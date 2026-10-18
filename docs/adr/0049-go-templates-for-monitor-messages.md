# ADR-0049: Monitor messages are Go text/template, proved at save

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

A notification message needs the value, the threshold, the group and the
status, and usually says something different for an alert than for a recovery.
Whatever language it is written in runs unattended, on the server, at the moment
something is already wrong.

## Decision

`text/template` with a fixed data type (`Monitor`, `Group`, `Status`,
`PrevStatus`, `Value`, `Threshold`, `IsAlert`, `IsWarning`, `IsRecovery`,
`IsNoData`, `Since`, `Link`). At save the template is parsed and **executed
against sample data for every status**, so a misspelt field is a 400 and not a
page that fails to render. `range`, `template`, `define` and `block` are
refused (nothing in the data is a list, and `{{range 1000000000}}` would be a
loop), output is capped at 8 KiB, and a missing *group* key renders empty. `@name`
mentions are read from the rendered text, so a template chooses its recipients.

## Alternatives considered

| Option | Why not |
|---|---|
| Datadog's `{{#is_alert}}` mustache dialect | A parser to write and maintain for a learning project whose point is elsewhere, with no standard behaviour to match. |
| Plain text with `$value`-style substitution | No conditionals, so no "check the workers" only on alert, and no per-status recipients. |
| `html/template` | Notifications are text, Discord markdown or email bodies; HTML escaping would mangle them. |

## Consequences

Authors write Go template syntax (documented with examples in
docs/operations.md). A template that compiles is guaranteed to render for every
status (a fuzz target holds that). A render failure at runtime (only possible
when real data prints past the cap) still sends a message saying so, with the
mentions kept.
