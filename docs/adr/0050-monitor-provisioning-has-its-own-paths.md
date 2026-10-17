# ADR-0050: Monitors are provisioned from their own directories

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The M6 spec says monitors are provisioned from `deploy/monitors/*.json` "like
dashboards", and the reference config listed `./deploy/monitors` under
`provisioning.paths` as an example. Dashboard provisioning reads *every*
`*.json` in those paths as a dashboard.

## Decision

A separate key, `provisioning.monitor_paths` (env
`OZY_PROVISIONING_MONITOR_PATHS`), lists monitor directories. Each provisioner
reads only its own. The image bakes `deploy/monitors/` into `/etc/ozy/monitors`.

## Alternatives considered

| Option | Why not |
|---|---|
| One list, each provisioner skipping files that look like the other kind | "Looks like" is a content sniff that both provisioners must agree on forever; a malformed monitor would be skipped silently by both. |
| One list, subdirectories per kind | Provisioning deliberately does not recurse (a work-in-progress sits next to the real files). |

## Consequences

An app that ships monitors mounts a directory and adds it to `monitor_paths`.
One more config key. The M6 spec is amended to name it.

A provisioned monitor whose file disappears is **released** (made API-managed,
with an event) rather than deleted or left provisioned. Deleting would lose
history to a directory that simply was not mounted; leaving it provisioned would
keep it running while the API refused every edit, pointing at a file that no
longer exists.
