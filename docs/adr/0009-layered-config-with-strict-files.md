# ADR-0009: Layered configuration with strict files and lenient environment

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

Both binaries need configuration that works four ways: with no config at all
(`go run ./cmd/ozyd`), from a checked-in YAML file, from environment
variables (containers), and — for the agent — from per-app fragments
(`deploy/agent.d/<app>.yaml`). The extensibility standard requires the last
one: an app is onboarded by adding a file, never by editing a shared one.

The layers must combine in a predictable way. Mistakes should fail loudly
rather than quietly running on a default. The plan's own rule is "unknown
config keys are an error (catches typos)".

## Decision

- **Precedence:** built-in defaults < main file < conf.d fragments <
  environment.
- **Files are strict.** An unknown key in any YAML file is an error that names
  the file and line. Each file is checked against the config's shape on its
  own before it is merged, so the line numbers are the ones in that file.
- **Fragments merge structurally.** Maps merge key by key and lists append in
  file-name order. A scalar set in two files is an error naming both files.
  `confd_path` may not be set inside a fragment.
- **The environment is lenient.** `PREFIX_` plus the upper-cased YAML path
  overrides any leaf. An unknown variable carrying the prefix is a *warning*
  logged at startup, not an error.
- The agent loads in two passes. The first reads the main file and the
  environment to learn `confd_path`. The second loads every layer.

## Alternatives considered

| Option | Why not |
|---|---|
| Last-writer-wins for fragments | The result then depends on file names. Two apps each setting `hostname` would silently race. Naming both files turns a mystery into a one-line fix. |
| Replace lists instead of appending | Each app's fragment needs to *add* tags, checks and log sources. Replacing would force every fragment to repeat the others. |
| Unknown environment variables are errors | This is consistent with files, but the environment is shared with other software. The SDKs use `OZY_AGENT_HOST`, which has the agent's prefix, so a shell configured for the SDK would stop the agent from starting. |
| Rename the agent prefix to avoid that collision | This would diverge from the plan's `OZY_AGENT_*` for one edge case. A warning handles it, and the warning still catches typos. |
| A config library (viper, koanf) | Merging, strictness and env mapping are ~200 lines here with exactly the semantics above. Viper's defaults are case-insensitive keys and last-writer-wins, the opposite of what we want. |
| Flags for every setting | This doubles the surface for no gain. Env already covers "override one thing". The CLI keeps just `-config` and `-version`. |

## Consequences

- A typo in a file fails startup with its location. A typo in an environment
  variable shows up as a startup warning. Operators should read the first log
  lines.
- Adding a setting is one struct field with a `yaml` tag. The environment name
  is derived from the tag, and `scripts/check-docs.sh` fails until the key
  appears in the reference YAML.
- Environment overrides support only scalar leaves and lists of strings.
  Structured lists, such as check instances in M3, must come from files. That
  is where they belong anyway.
