# ADR-0029: A restarted agent writes no bucket its predecessor did

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

At shutdown the agent's aggregator does a final flush of every open bucket,
so nothing it received is lost on the way out. The agent that replaces it
(a restart, `make up`) starts aggregating at once, and its first flush
writes the bucket it started in: the same timestamp the final flush just
wrote, for every context both agents saw. ozyd keeps one point per series
per timestamp, refuses the second as out of order, and the new agent's
first interval for those series is lost, counted in the forwarder's
rejects.

M1 accepted this, thinking the second point would replace the first. It
does not, and it was rare while only statsd clients sent samples. The docker
event watcher (M3 §3) made it certain: a host running short-lived
containers (app-python's judge sandboxes exit about 80 times per 20s) has a
`container.exits` context in every bucket, so every agent restart produced
refused points and smoke's "no samples were dropped" check failed.

## Decision

The aggregator takes the agent's start time (`Options.Started`) and stamps
no bucket before its **floor**: the first bucket that begins strictly after
the start second, `floorTo(start) + interval`. A sample that arrives before
the floor, or is stamped earlier, is counted in the floor bucket instead:
seconds late, not lost. "Strictly after" covers an agent started in the
very second its predecessor stopped, when that second is a boundary.

The agent's self-metrics reporter, whose points ride with each flush,
takes the same floor. It reports nothing stamped before the floor, and
what it counted there is reported at the first bucket after it.

The final flush at shutdown emits every open bucket **except those that
start after now**. The next agent's floor is the bucket after the second it
starts in, which can be any bucket after this agent's now, so a bucket ahead
of now may be the next agent's first. Those buckets are dropped. They hold:

- the samples of an agent stopped within its first interval, which the
  floor moved forward to a bucket that has not begun;
- samples from a client whose clock runs fast (a `|T` timestamp within the
  60s tolerance, ahead of receipt).

## Alternatives considered

| Option | Why not |
|---|---|
| Keep M1's behaviour: the new agent writes the bucket again | ozyd refuses the second point. Losing the new agent's first interval on every restart is worse than the rare losses above. |
| Skip only the floor bucket at the final flush, and emit other future buckets | Round 2 of PR #35's review proposed this, to keep a fast client's samples. Round 3 showed the case it reopens: an agent stopped at 135 emits bucket 140 for a client 8s fast, and its successor starting at 136 has floor 140 and is refused. |
| Have ozyd accept a second point at a timestamp by adding (counts) or replacing (gauges) | That is the right fix in the long run, but it is a storage change (the head's append path, WAL records, compaction's dedupe) and it changes what the store means by a point. It is out of scope for M3. This ADR would be superseded by it. |
| Persist the last flushed bucket on disk and have the new agent start after it | That adds state to a component that has none, and a disk path to the agent. It also still loses the new agent's first partial interval. |

## Consequences

- Restarts no longer produce refused points. Smoke's zero-drop check holds
  through an agent restart with container churn running.
- A sample that reaches a new agent in its first partial interval is
  stamped up to one interval late. For a counter, the floor bucket
  therefore holds up to two intervals' increments under one interval's
  width, and a per-second view of it reads up to twice the true rate for
  that one bucket after each restart. That is accepted: the total is
  right, the alternative is to lose the samples, and a series' interval
  is shared by all its points in a flush, so stretching it for one bucket
  would misstate the others.
- An agent stopped within its first interval loses what it received, and
  a fast client clock loses its future-stamped samples at shutdown. Both
  are rare. Neither can be a self-metric, because the loss happens as the
  process exits, after its last report. The agent logs it instead: "final
  flush held back buckets that had not begun", with the number of buckets.
- Supersede this when ozyd accepts a second point at a timestamp.
