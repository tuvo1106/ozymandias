# ADR-0032: A check's labels are its own settings, with no shortcuts

- **Status:** Accepted
- **Date:** 2026-10-01

## Context

M3 §3 gives two examples of autodiscovery labels: `ozy.check.redis.port=6379`
and `ozy.check.openmetrics.path=/metrics`. The first is a redis setting. The
second is not an openmetrics setting: the check takes a whole `url`, because a
configured instance scrapes any page, anywhere. A container labelled as the
spec shows is refused, since a check decodes its settings strictly (a
misspelt key fails rather than being ignored), and the refusal is logged and
counted in `ozy.agent.autodiscovery.errors`. Either the check grows a `path`
or the spec is wrong.

## Decision

A label `ozy.check.<check>.<setting>` sets exactly the setting
`collectors.checks.<check>.instances[].<setting>`, with `%%host%%` and
`%%port%%` filled in from the container. There are no label-only settings.
The openmetrics label is
`ozy.check.openmetrics.url=http://%%host%%:%%port%%/metrics`. The spec is
amended to say so.

## Alternatives considered

| Option | Why not |
|---|---|
| A `path` setting, with the URL built as `http://%%host%%:%%port%%<path>` when `url` is absent | Two ways to say one thing, and a setting meaningless in a config file (there is no `%%host%%` there): either it is refused when configured, or it means something different there. Scheme and port also need their own shortcuts the first time an exporter serves https or a second port. |
| Labels with their own vocabulary, translated per check | Every check would document two sets of settings, and the two would drift. The rule "a label is a setting" needs no per-check documentation at all. |

## Consequences

- One reference for both: a check's settings table in docs/operations.md
  says what a label can set.
- A label is slightly longer for openmetrics (`url=http://%%host%%:%%port%%/metrics`
  rather than `path=/metrics`).
- Impact on later milestones: none. M4's log autodiscovery, if it comes, takes
  the same rule.
