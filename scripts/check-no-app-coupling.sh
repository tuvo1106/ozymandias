#!/usr/bin/env bash
# Enforces docs/plan/extensibility.md §1: the core never names a specific
# instrumented app. App-specific material lives only in deploy/, examples/ and
# docs/ — never in code that ships to someone else's app.
set -euo pipefail
cd "$(dirname "$0")/.."

dirs=()
for d in cmd internal pkg sdk web/src; do [[ -d $d ]] && dirs+=("$d"); done

if [[ ${#dirs[@]} -eq 0 ]]; then echo "✓ no core code yet"; exit 0; fi

pattern='comic[-_ ]?board|leeter[-_ ]?code|boba[-_ ]?gals'
if matches=$(grep -rniE "$pattern" "${dirs[@]}" --exclude-dir=node_modules --exclude-dir=dist); then
  echo "✗ app-specific names found in the core (docs/plan/extensibility.md §1):"
  echo "$matches"
  exit 1
fi
echo "✓ no app coupling in: ${dirs[*]}"
