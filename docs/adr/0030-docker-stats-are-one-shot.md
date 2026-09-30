# ADR-0030: Docker stats are one-shot, and containers are listed each run

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

The M3 spec for the docker collector says:
- read `GET /containers/{id}/stats?stream=false` per container;
- compute CPU % from that response's own two samples (`precpu_stats` →
  `cpu_stats`);
- refresh the container list "every 10s and on `/events` start/die".

With `stream=false` alone, the daemon samples the cgroup twice, a second
apart, and holds the request for that second. At the default of eight
requests in flight, a host with a hundred containers takes over twelve
seconds to read. That is most of the 15s interval, and close to the 10s
run timeout. Review of PR #35 found this.

## Decision

- **One-shot stats.** Stats are read with `one-shot=true` (API 1.41, Docker
  20.10), which answers at once with a single sample.
- **CPU % between runs.** The collector keeps each container's previous
  `cpu_stats` and computes CPU % between the two runs' samples, so its
  window is the collection interval (15s) rather than one second. A
  container's first run has no CPU value.
- **Resetting the baseline.** A container restarted in place keeps its id,
  so the event watcher's start event resets its baseline.
- **Listing.** Containers are listed at the start of each run, not on a
  separate 10s timer or on start/die. The list is only used by the run that
  follows it, and the event stream already covers what a poll cannot see:
  exits and lifetimes, through the watcher.

## Alternatives considered

| Option | Why not |
|---|---|
| `stream=false` without one-shot (the spec) | Every request costs a second of the daemon's time, and runs grow with the container count. |
| Keep `stream=true` open per container | This means one long-lived connection and goroutine per container, and the collector would own their lifecycle. That is a larger design for the same number. |
| A separate list refresh, every 10s and on start/die | The only thing that reads the list is a run, which lists anyway. A refresh between runs is work nothing uses. |

## Consequences

- A run costs about one quick request per container. `max_concurrency`
  bounds the load, not the run's length.
- CPU % is averaged over the interval, not over one second. It is a
  smoother figure, and it matches `container.net.*` and `container.io.*`,
  which are rates over the interval too.
- A container needs two runs before its CPU is reported, and it needs two
  again after an in-place restart.
- API versions older than 1.41 ignore `one-shot` and still answer with two
  samples, a second late. The collector still works with them, but slower.
  This package no longer decodes `precpu_stats`.
