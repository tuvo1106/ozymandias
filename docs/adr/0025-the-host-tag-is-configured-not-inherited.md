# ADR-0025: The host tag is configured, not inherited from the OS

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

ozyd tags its own metrics `host:<name>` and the agent tags everything it
sends the same way. Both took the name from `os.Hostname` unless told
otherwise, and only the agent could be told otherwise (`hostname`,
`OZY_AGENT_HOSTNAME`). Three things turned out to make the OS name
unstable:

- **In a container** it is the container id, new every time compose
  recreates it. After a day of `make up` the Metric Summary page (#31)
  showed ozyd's self-metrics under 24 `host` values, each carrying a full
  copy of every self-metric series.
- **On macOS, natively,** Go's `os.Hostname` is `Name.local` while
  `hostname -s` is `Name`, so ozyd and the agent under `make dev` disagreed.
- **On macOS, `hostname -s` itself** comes from DHCP or reverse DNS when the
  network supplies a name. The same laptop reported `Tus-MacBook-Pro` and,
  on another network, `Mac` (from `Mac.attlocal.net`). The stored values
  already held both.

A host tag that changes when nothing about the host changed defeats its
purpose: it splits one machine's history into several series and counts
against the per-metric series limit each time.

## Decision

- ozyd gets a `hostname` setting (`OZY_HOSTNAME`), mirroring the agent's.
  Empty still means the OS hostname, so a bare `ozyd` behaves as before.
- One function, `config.ResolveHostname`, turns "configured, else OS" into a
  name and a normalized tag for both binaries. A configured name that cannot
  be a tag fails config validation (exit 2). Before this, it became an
  empty tag, and the intake then refused every self-metric series.
- The launchers decide the name once, in `scripts/hostname.sh`: an exported
  `OZY_HOSTNAME`, else on macOS `scutil --get LocalHostName` (the machine's
  own setting, independent of the network), else `hostname -s`. `make up`,
  `make dev` and `make smoke` all use it and pass the same value to both
  processes.

## Alternatives considered

| Option | Why not |
|---|---|
| Set compose's `hostname:` on the ozyd container (this PR's first version) | Fixes compose only. `make dev` still had ozyd and the agent disagreeing, and a container hostname is a side channel: nothing in ozyd's config says where the tag comes from |
| Keep `hostname -s` in the launchers | It changes with the network on macOS, which is the same instability one layer down |
| Drop `host` from ozyd's self-metrics | ozyd is one process per deployment today, but the tag is how a dashboard filters "this machine", and the agent's data carries it. Removing it would make the two sources impossible to join |
| Normalize or truncate a bad configured name instead of refusing it | The tag would silently differ from what the operator wrote, and two machines could collapse onto one value |

## Consequences

- One machine is one `host` value across restarts, recreates and networks,
  as long as it is launched through `make` (or with `OZY_HOSTNAME` set).
- Under `make up`/`make dev` the environment always sets the name, so
  `hostname:` in `deploy/ozyd.yaml` has no effect there. The file says so.
- Values recorded before this change stay until retention removes them.
- Renaming a machine (its LocalHostName) still starts a new host value.
  That is correct: the operator changed the name.
