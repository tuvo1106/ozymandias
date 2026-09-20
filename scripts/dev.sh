#!/usr/bin/env bash
# `make dev`: ozyd + agent as native processes (from ./bin) plus the Vite
# dev server with hot reload on :9401. Ctrl-C stops all three.
#
# Running natively (rather than in compose) is also how a process on the Mac
# reaches the agent's statsd UDP port under Colima — see docs/operations.md.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p data
pids=()
# Job control, so each child below lands in its own process group. Without it
# they share this script's group, Ctrl-C reaches all three at once, and the
# shutdown order this script exists to impose never happens — ozyd starts
# draining at the same instant the agent starts its final flush.
set -m

# Stop in reverse pipeline order: the agent's final flush needs ozyd
# still listening, or its last buckets are lost (compose gets the same order
# from depends_on). The `:-` defaults matter under `set -u`: the trap is armed
# before the three processes exist, so an early Ctrl-C would otherwise abort
# cleanup on an unbound element and leave whatever did start running.
cleanup() {
  trap - INT TERM EXIT
  kill -TERM "${pids[1]:-}" "${pids[2]:-}" 2>/dev/null || true
  wait "${pids[1]:-}" "${pids[2]:-}" 2>/dev/null || true
  kill -TERM "${pids[0]:-}" 2>/dev/null || true
  wait "${pids[0]:-}" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

./bin/ozyd -config deploy/ozyd.yaml & pids+=($!)
OZY_AGENT_HOSTNAME=${OZY_AGENT_HOSTNAME:-$(hostname -s)} \
  ./bin/agent -config deploy/agent.yaml & pids+=($!)
# exec: the tracked pid is vite itself, not an npm wrapper that would
# swallow the signal and leave vite running.
(cd web && exec node_modules/.bin/vite) & pids+=($!)

echo "ozyd http://localhost:9400 · agent http://localhost:8126 · UI (hot reload) http://localhost:9401"
# Stop everything as soon as any one process exits. A polling loop rather
# than `wait -n`, which macOS's bash 3.2 doesn't have.
while :; do
  for pid in "${pids[@]}"; do
    kill -0 "$pid" 2>/dev/null || exit 1
  done
  sleep 1
done
