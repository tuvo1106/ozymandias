# ADR-0054: A public instrumented app may be named

- **Status:** Accepted
- **Date:** 2026-10-23
- **Narrows:** [ADR-0012](0012-public-repo-and-the-name.md)'s rule that the public tree refers
  to the instrumented apps only by role

## Context

ADR-0012 moved "app-integration detail that names the owner's private apps" into the gitignored
`docs/private/integrations.md`, and the public tree calls the apps `app-node`, `app-python` and
`app-ruby`. The reason was privacy: those apps were private.

The Python app is now shikomi, a public repository. ADR-0037 and the M3 notes already name it, and
M6 cannot avoid naming it: a shipped monitor's query must select the service name the app
reports (`service:shikomi-api`). A query cannot take a variable the way a dashboard's template
does. Review of #61 found AGENTS.md §6 and this practice in conflict.

## Decision

**An instrumented app whose own repository is public may be named in `deploy/`, `scripts/demo/`,
`docs/notes/` and ADRs.** Its role name stays the name in specs, `docs/plan/` and prose about
integration in general.

Unchanged:
- **A private app is still referred to by role only.** Its name is still denied by
  `scripts/check-no-app-coupling.sh` via `docs/private/app-names.txt`.
- **No app's name may appear in the core** (`cmd`, `internal`, `pkg`, `sdk`, `web/src`) or in
  tests (docs/plan/extensibility.md). The coupling check enforces this for every app.
- **Integration points, local paths and private notes stay** in `docs/private/integrations.md`.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep role names everywhere and move the app-python monitors and demo script out of the repo | The monitors are M6's shipped evidence and must run against the real service name. Moving them hides what the acceptance runs used, to protect a name that is already public. |
| Make the service name configurable in monitor files | A monitor has no variables, and adding them for one app's name is a grammar change with no other user. |

## Consequences

- AGENTS.md §6 says this.
- If an app goes private again, its name moves to `docs/private/app-names.txt`, and the files named
  above need a pass.
