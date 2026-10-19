# ADR-0052: A monitor's notifiers come from its template source, not its rendered text

- **Status:** Accepted
- **Date:** 2026-10-18

## Context

The M6 spec says "`@handle` mentions in the message select notifiers". The
first implementation read the mentions from the *rendered* message, so that
`{{if .IsAlert}}@pager{{end}}` paged only for alerts. But rendered text holds
data: a tag value, a log attribute or a monitor name containing `@pager` could
page someone. Three review rounds tried to tell the author's mentions from the
data's within rendered text, and each round's rule had a hole:

- intersecting with the source's handles let data *repeat* a handle the
  template had hidden behind a condition;
- re-rendering with `@` stripped from data made routing and the sent text
  disagree (a log monitor's group key is itself `@env`, so a condition on
  it flipped);
- escaping `@` in data in the one real render changed what templates
  computed (`eq .Group.user "a@b"` was never true), corrupted addresses in
  the sent text, and let a large attribute push the render past its cap,
  whose fallback reached everyone;
- every boundary rule for "a mention in the text" disagreed with the
  save-time rule for some template (`{{.Value}}{{if .IsAlert}}@pager{{end}}`
  renders `42@pager`).

## Decision

A message goes to every handle its template mentions, whatever the status
and whatever the data. Mentions are read once, when the template compiles,
from its literal text only, by walking the parse tree: `@handle` at the start
of the message or after whitespace. `@handle` directly after `}}` (an action
or a comment, which may be trimmed) is refused, because what it reads as
depends on what came before it. The error gives both ways out:
a space for a mention, `{{"@"}}` for a literal `@`. A comment
(`{{/* @pager */}}`) or a string literal mentions nobody. Save-time
validation uses the same function. A mention inside `{{if}}` or `{{with}}`
is refused at save, because it would be sent for every status while reading
as if it were not. Data is rendered as it is.

## Alternatives considered

| Option | Why not |
|---|---|
| Mentions in the rendered text, with the defences above | Four rounds of holes; each fix made another. Routing that depends on data is a security property that has to be argued about every template. |
| An explicit per-status recipient option (`notify: {alert: [pager], ok: [log]}`) | The likely future shape if per-status routing is wanted, and safe, since it is not text. Not in the M6 spec; additive later. |

## Consequences

One audience per status is one monitor per audience (or the future option
above).

Impact on later milestones: the M6 UI's monitor editor can show recipients
statically from the template (the validate endpoint's `mentions`), with no
preview per status. The app integration in M6 PR 3 writes one monitor per
audience where it wants a pager only for alerts. Nothing in M7 or M8 depends
on routing. A template that mentions `@pager` pages it for recoveries and
warnings too, which docs/operations.md says plainly. What routes is visible in
the template source and in `GET /api/v1/monitors` without rendering anything,
and no value an outsider can send changes it.
