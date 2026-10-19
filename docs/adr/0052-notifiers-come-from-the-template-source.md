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

A message goes to every handle its template source mentions, whatever the
status and whatever the data. Mentions are read once, when the template
compiles, with the same rule save-time validation uses. Data is rendered as
it is. A mention inside `{{if}}` still counts.

## Alternatives considered

| Option | Why not |
|---|---|
| Mentions in the rendered text, with the defences above | Four rounds of holes; each fix made another. Routing that depends on data is a security property that has to be argued about every template. |
| An explicit per-status recipient option (`notify: {alert: [pager], ok: [log]}`) | The likely future shape if per-status routing is wanted, and safe, since it is not text. Not in the M6 spec; additive later. |

## Consequences

One audience per status is one monitor per audience (or the future option
above). A template that mentions `@pager` pages it for recoveries and
warnings too, which docs/operations.md says plainly. What routes is visible in
the template source and in `GET /api/v1/monitors` without rendering anything,
and no value an outsider can send changes it.
