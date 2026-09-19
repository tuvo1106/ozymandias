#!/usr/bin/env bash
# Cheap drift checks for the documentation standard
# (docs/plan/documentation.md §5). Each check is a grep, deliberately: they are
# meant to be obviously correct and to fail loudly, not to be clever.
#
#   1. Every Go package under internal/ and pkg/ has a doc.go.
#   2. Every HTTP route pattern registered in Go code appears in docs/api.md.
#   3. Every `yaml:"…"` config key appears in its commented reference YAML.
#   4. Every self-metric name registered in Go code (Counter, Gauge,
#      GaugeFunc) appears in docs/metrics-catalog.md.
#   5. Relative links in tracked Markdown files resolve.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
err() { echo "✗ $*"; fail=1; }

# --- 1. doc.go per package -------------------------------------------------------
while read -r dir; do
  [[ -f "$dir/doc.go" ]] || err "$dir has Go files but no doc.go"
done < <(find internal pkg -name '*.go' ! -name '*_test.go' -print0 2>/dev/null |
  xargs -0 -n1 dirname | sort -u)

# --- 2. routes documented --------------------------------------------------------
# Go 1.22 mux patterns: "GET /path/{id}". Only the path is required in the doc.
while read -r route; do
  grep -qF -- "$route" docs/api.md || err "route $route is registered but missing from docs/api.md"
done < <(grep -rhoE '"(GET|POST|PUT|PATCH|DELETE) /[^" ]*"' cmd internal --include='*.go' --exclude='*_test.go' 2>/dev/null |
  sed -E 's/"[A-Z]+ ([^"]*)"/\1/' | sort -u)

# --- 3. config keys documented ---------------------------------------------------
check_config() { # <go package dir> <reference yaml>
  local dir=$1 ref=$2 key
  [[ -d $dir ]] || return 0
  while read -r key; do
    grep -qE "^[[:space:]]*#?[[:space:]]*${key}:" "$ref" ||
      err "config key '$key' ($dir) is missing from $ref"
  done < <(find "$dir" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec grep -hoE 'yaml:"[a-z0-9_]+' {} + |
    sed 's/yaml:"//' | sort -u)
}
check_config internal/config deploy/ozyd.yaml
check_config internal/agent/config deploy/agent.yaml

# --- 4. self-metrics catalogued --------------------------------------------------
while read -r name; do
  grep -qF -- "\`$name\`" docs/metrics-catalog.md || err "metric $name is emitted but missing from docs/metrics-catalog.md"
done < <(grep -rhoE '(Counter|Gauge|GaugeFunc)\("ozymandias\.[a-z0-9_.]+"' cmd internal --include='*.go' --exclude='*_test.go' 2>/dev/null |
  sed -E 's/.*\("([^"]+)"/\1/' | sort -u)

# --- 5. relative Markdown links resolve -----------------------------------------
root=$(pwd)
while read -r md; do
  mddir=$(dirname "$md")
  while read -r target; do
    target=${target%%#*}
    [[ -z "$target" ]] && continue
    # Lexical normalisation (the target may not exist); BSD realpath has no -m.
    resolved=$(python3 -c 'import os,sys;print(os.path.normpath(os.path.join(sys.argv[1],sys.argv[2])))' "$root/$mddir" "$target")
    # Links that leave the repo (e.g. ../app-node) point at sibling
    # checkouts that don't exist in CI; they are not ours to verify.
    [[ "$resolved" == "$root"/* ]] || continue
    [[ -e "$resolved" ]] || err "$md links to missing $target"
  done < <(grep -oE '\]\([^)[:space:]]+\)' "$md" | sed -E 's/^\]\((.*)\)$/\1/' |
    grep -vE '^(https?:|mailto:|#)' || true)
done < <(git ls-files '*.md')

if [[ $fail -eq 0 ]]; then echo "✓ docs checks passed"; fi
exit "$fail"
