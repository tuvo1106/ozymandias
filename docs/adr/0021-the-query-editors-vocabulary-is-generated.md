# ADR-0021: The query editor's vocabulary is a generated file, not an endpoint

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

The dashboard editor completes metricql as it is typed: aggregators, function
names, modifiers, and the arguments of `.rollup()` and `.fill()`. Those words
are the parser's (`internal/query/metricql`), and the editor must offer
exactly them — a word the editor offers and the parser refuses is a parse error
the editor typed, and a word the parser accepts and the editor never offers
looks, to the author, like a word that does not exist.

`metricql.FunctionNames()` already existed "for the query editor's completion
list", but nothing carried it to the browser, and the modifiers had no list at
all — only a `switch` in the parser.

## Decision

`metricql.Words()` returns the vocabulary from the parser's own tables, and a
test in that package writes it to `web/src/lib/metricqlVocabulary.json`
(`-update-vocabulary`) and fails when the committed copy differs. The UI
imports the JSON at build time.

Modifiers get a table (`modifierWords`) for the list to read from. Because the
parser still dispatches with a `switch`, a second test compares that table
with the modifiers named in the parser's own refusal, in both directions, and a
third parses every word in its position.

## Alternatives considered

| Option | Why not |
|---|---|
| `GET /api/v1/query/vocabulary` | The UI is embedded in the same binary as the parser (`go:embed`), so the two are always one version and a request can only return what the build already knew. It would add a "not loaded yet" and a "failed to load" state to every completion — two more missing values for the editor to render — to carry no information. |
| A hand-written list in TypeScript | It drifts the day an aggregator is added, silently, which is the failure this exists to prevent. |
| Parse the Go source from the web build | A build step that reads another language's source is a second parser of that language's syntax, and breaks on a refactor that changes nothing about the vocabulary. |

## Consequences

- Adding an aggregator, function or modifier fails `go test` until the JSON is
  regenerated, so the editor learns the word in the same commit the parser does.
- The web build does not need a running `ozyd` for its completion lists, and the
  editor's tests exercise the real vocabulary rather than a fixture.
- `make dev` (Vite against a separately built `ozyd`) can, in principle, pair a
  bundle with a parser from another commit. That was already true of every
  other contract between them, and the check runs in CI on each commit.
- A test in `internal/query/metricql` writes into `web/`. It is the one place
  that can see both sides, which is the point; it only writes under the flag.
