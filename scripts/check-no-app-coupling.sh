#!/usr/bin/env bash
# Enforces docs/plan/extensibility.md §1: the core never names a specific
# instrumented app. App-specific material lives only in deploy/, examples/ and
# docs/ — never in code that ships to someone else's app.
#
# Two denylists. The built-in one is the role tokens the public docs use, so
# this check still means something on a clean clone and in CI. The second is
# docs/private/app-names.txt (gitignored): the owner's real app names, which
# are deliberately not in this repo. If that file is absent the extra patterns
# are simply skipped — nothing to leak, nothing to check.
set -euo pipefail
cd "$(dirname "$0")/.."

dirs=()
for d in cmd internal pkg sdk web/src; do [[ -d $d ]] && dirs+=("$d"); done

if [[ ${#dirs[@]} -eq 0 ]]; then echo "✓ no core code yet"; exit 0; fi

pattern='app-node|app-python|app-ruby'
private=docs/private/app-names.txt
if [[ -f $private ]]; then
  while IFS= read -r line; do
    [[ -z $line || $line == \#* ]] && continue
    pattern="$pattern|$line"
  done < "$private"
else
  echo "note: $private absent; checking the public role tokens only"
fi

if matches=$(grep -rniE "$pattern" "${dirs[@]}" --exclude-dir=node_modules --exclude-dir=dist); then
  echo "✗ app-specific names found in the core (docs/plan/extensibility.md §1):"
  echo "$matches"
  exit 1
fi
echo "✓ no app coupling in: ${dirs[*]}"
