# ADR-0012: The project is called ozymandias, and the repo is public

- **Status:** Accepted
- **Date:** 2026-09-23

## Context

The working name this project carried while the repo was private was a pun on
a commercial observability product. That was fine in private and is not fine to
publish: the product's name is a live trademark, and a name that exists only as
a play on it invites a confusion claim no learning project should have to
answer. The concern is trademark, not copyright — an audit before the move
found no copied text, code, metric tables or documentation from any commercial
product anywhere in the tree — and the cheapest time to fix a name is before
anyone depends on it.

Staying private had two other costs. GitHub Actions minutes are metered on the
Free plan and were exhausted (ADR-0010), and the SDKs could not be published
(ADR-0007). Both constraints are artifacts of visibility, not of design.

## Decision

The project is **ozymandias** — Ramesses II's Greek name, and the title of
Shelley's 1818 sonnet. Both are public domain. The repo is public.

Two tokens, deliberately:

| Where | Token |
|---|---|
| Repo, Go module, README, prose | `ozymandias` |
| Anything typed — binary, env vars, metrics, headers, SDK packages | `ozy` |

So: `github.com/tuvo1106/ozymandias`; binaries `ozyd` and `agent`; env prefixes
`OZY_` and `OZY_AGENT_`; self-metrics `ozy.*`; wire headers `X-Ozy-*`; SDK
packages `ozy`; config at `/etc/ozy`; data at `./data/ozyd`. Ten letters is too
many for `OZY_AGENT_FORWARDER_SHUTDOWN_TIMEOUT` to become
`OZYMANDIAS_AGENT_FORWARDER_SHUTDOWN_TIMEOUT`, and three is too few to name a
project.

Two consequences follow from publishing rather than from the name:

- **The on-disk block magic changed** from `DDCH`/`DDIX` to `OZCH`/`OZIX`
  (`docs/formats/block.md`). The old bytes were an abbreviation of the old
  name; leaving them would have been the one reference nothing explained.
  Blocks written by earlier builds are unreadable here, which matters to
  nobody: the only data that ever existed was local test data.
- **No commercial product is used as a landmark.** The comparison passages in
  the learning notes and package docs name Prometheus, Loki and "production
  backends" instead. Four references survive because removing them would make
  the repo inaccurate: the `datadogpy` and `hot-shots` client names in
  `scripts/capture-statsd-compat.sh` and its captures (we install and run
  them), `DataDog/sketches-go` in AGENTS.md's banned-dependency list (a ban has
  to name its target), and the DDSketch and `dd-trace-py` citations in PLAN.md's
  reading list. The wire dialect is described as "extended StatsD", which is
  what it is, rather than by the vendor name for the same grammar.

App-integration detail that names the owner's private apps moves to a
gitignored `docs/private/integrations.md`. The public tree refers to the three
instrumented apps by role — `app-node`, `app-python`, `app-ruby`.

The original private repo stays as the archive of record for that file and for
the true commit history. This repo's history is four commits, one per milestone
boundary, rebuilt from those trees under the new name.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep the old working name and publish anyway | The name is the whole exposure, and it costs nothing to change now. |
| `galactus` | Coined by Marvel in 1966 and actively enforced. It trades one publisher's mark for another's, and the newer one is the invented word rather than the borrowed one. |
| `ozymandias` everywhere, no short token | `OZYMANDIAS_AGENT_FORWARDER_SHUTDOWN_TIMEOUT` and `ozymandias.agent.aggregator.flush_duration_ms` are typed in every query and every compose file. |
| Rewrite all 69 commits with `git-filter-repo` | Preserves a narrative that `docs/notes/M0–M2.md` already tells better, and the rewritten trees would not build, since text substitution cannot regenerate binary goldens. |
| Start from a single squashed commit | Loses the milestone boundaries, which are the one structure in this history worth keeping. |
| Keep the repo private and just rename | Leaves the Actions and publishing constraints in place for no gain. |

## Consequences

Anyone running the old binaries must re-create their config: every env var,
the config paths and the data directory changed. There is no migration path
for on-disk blocks, by choice.

`docs/private/integrations.md` exists only on the owner's machine and in the
archived repo. It needs backing up like any other unversioned file, and a new
contributor will find the references to it dangling — deliberately.

Actions now runs on pull requests (ADR-0013) and the SDKs can be published
whenever the API is stable (ADR-0014). `SECURITY.md` and `CODE_OF_CONDUCT.md`
ship with the repo, which PLAN.md previously named as a reason to stay private.
