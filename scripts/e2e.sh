#!/usr/bin/env bash
# `make e2e`: start the dev stack (scripts/dev.sh), seed synthetic traces, run the Playwright
# tests, stop the stack. The stack always stops, pass or fail, and so does the data dir it used.
set -euo pipefail
cd "$(dirname "$0")/.."

for port in 9400 9401 8126; do
  if lsof -nP -iTCP:$port -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port is busy; stop whatever uses it (make down, or the dev stack) first" >&2
    exit 1
  fi
done

(cd web && npx playwright install chromium >/dev/null)
scripts/dev.sh >/tmp/ozy-e2e-stack.log 2>&1 &
stack=$!
trap 'kill -TERM $stack 2>/dev/null || true; wait $stack 2>/dev/null || true' EXIT

for _ in $(seq 1 60); do
  curl -fsS localhost:9400/healthz >/dev/null 2>&1 && curl -fsS localhost:9401/ >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS localhost:9400/healthz >/dev/null || { echo "ozyd did not start; see /tmp/ozy-e2e-stack.log" >&2; exit 1; }

python3 scripts/seed-traces.py --label "e2e-$(date +%s)" --count 40
(cd web && npx playwright test)
