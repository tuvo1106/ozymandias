# ADR-0036: Container names are capped per agent

- **Status:** Accepted
- **Date:** 2026-10-04

## Context

`container_name` is a tag on every `container.*` series and on `container.exits` and
`container.lifetime`, and nothing bounds the names a Docker daemon has. Every unnamed
`docker run` gets a generated one (`admiring_allen`), and each exit mints a series. On the
development machine's shared Docker VM, `container.exits` reached 259 series across 211 names,
almost all generated, with no end in sight. `container_name_rewrite` (the judge-sandbox fix) is the precise remedy, but it only helps for patterns an operator
predicted.

## Decision

**The agent admits at most `collectors.docker.max_container_names` distinct names (default 200;
0 is unlimited) and reports every container past that as `container_name:other`, without a
`container_id`.** One `NameCap` is shared by the Docker collector, the event watcher and
autodiscovery, so a container has one name everywhere. `ozy.agent.docker.container_names_folded`
counts folded containers.

- Admission is first come, first kept, for the life of the process: a name that changed to
  `other` mid-life would split one container's series in two.
- Names that a `container_name_rewrite` rule groups do not count and are never folded: the rule
  already said "these are one thing".

## Alternatives considered

| Option | Why not |
|---|---|
| A default rewrite rule for Docker's generated names | The shape `word_word` also matches real names (`redis_cache`); shipping it would silently merge a user's services. |
| Evict dead names to free slots | The TSDB keeps the old series regardless, so eviction bounds nothing, and a reused slot makes two containers share a name over time. |
| Document it and do nothing in code | Costs nothing, but leaves the cardinality of an agent metric set by whatever runs on the daemon. |
| Cap in the server (ozyd) | The agent is where the name is chosen and where `container_id` is dropped; the server would cap series it cannot rename. |

## Consequences

- A long-running agent on a churning daemon fills the set and then folds every new name, including
  a real service started later, until restart. The fold counter is the signal; the fix is a
  rewrite rule or a higher limit.
- `other` is a name a container could really have; it is not reserved.
- Folded containers' `container.*` values sum into one series like a rewritten group's.
- The cap lives in the agent, so a cap change needs an agent restart.
