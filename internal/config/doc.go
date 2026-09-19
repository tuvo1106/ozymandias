// Package config loads ozymandias's YAML configuration and defines the
// ozyd server's settings. The agent's settings live in
// internal/agent/config and use the same loader.
//
// A setting's value is decided in layers, each overriding the last:
//
//	defaults  <  main YAML file  <  conf.d fragments  <  environment
//
// In detail:
//
//   - Defaults come from the Default constructor, so a zero-config
//     `ozyd` runs.
//   - The main file is the one given by -config. Unknown keys are an error,
//     reported with file and line: a typo like `adr:` for `addr:` must not
//     silently leave the default in place.
//   - conf.d fragments (agent only) are every *.yaml in a directory, merged
//     in lexical order. They exist so each app can ship its own file
//     (deploy/agent.d/<app>.yaml) without editing a shared one. Maps merge,
//     lists append, and a scalar set in two places is an error naming both
//     files — last-writer-wins would make the result depend on file names.
//   - Environment variables override any leaf: the YAML path upper-cased and
//     joined with "_" behind a prefix, e.g. OZY_HTTP_ADDR. This is what
//     containers use. Unlike the files, an unknown variable with the prefix is
//     only a warning: the environment is shared with other software (the SDKs'
//     OZY_AGENT_HOST shares the agent's prefix), so rejecting it would
//     make the agent unstartable in an otherwise valid shell.
//
// See docs/adr/0009-layered-config-with-strict-files.md for the alternatives
// considered, and deploy/*.yaml for the fully commented reference files.
package config
