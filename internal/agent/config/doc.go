// Package config defines the agent's settings (deploy/agent.yaml) and loads
// them with the shared layered loader in internal/config, adding the one thing
// only the agent has: a conf.d directory of fragments.
//
// The fragment directory is itself a setting (confd_path), which makes loading
// two-phase: first the main file and environment are read just to learn
// where conf.d is, then everything is loaded again with the fragments in
// place, so the documented precedence (defaults < file < fragments < env)
// holds for every key.
//
// What goes in a fragment? From M3 on: check instances, log sources, tag and
// rename rules — anything an app needs from the agent. Each app gets its own
// file (deploy/agent.d/<app>.yaml), which is how apps stay configuration
// rather than code (docs/plan/extensibility.md §1).
package config
