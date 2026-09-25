#!/usr/bin/env bash
# Build both SDKs and drop the artifacts into the instrumented apps' vendor
# directories (`make sdk-release`).
#
#   ./scripts/release-sdk.sh [app...]     # default: every app that exists
#
# Why vendored tarballs rather than a registry: the SDKs are pre-1.0 and these
# SDKs are not published anywhere. A packed artifact is also the honest test —
# it exercises the real `files` list and the built `dist/`, so a file missing
# from the package fails here rather than inside the app. `npm link` was the
# obvious alternative and is rejected: symlinked packages and Turbopack do not
# get along (docs/private/integrations.md §1).
#
# The apps are siblings of this repo and are not required to exist; an absent
# one is skipped, not an error.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)

NODE_VERSION=$(node -p "require('$ROOT/sdk/node/package.json').version")
PY_VERSION=$(grep -m1 '^version' sdk/python/pyproject.toml | sed -E 's/.*"(.*)".*/\1/')
PY_NAME=$(grep -m1 '^name' sdk/python/pyproject.toml | sed -E 's/.*"(.*)".*/\1/')

# app → language, so each app gets only the artifact it can use.
apps_node="app-node"
apps_python="app-python"

mkdir -p dist   # npm pack --pack-destination does not create it

echo "→ building sdk/node@$NODE_VERSION"
(cd sdk/node && npm ci --silent && npm run --silent build)
# --ignore-scripts: prepack would rebuild what we just built. npm prints the
# tarball name on stdout and its notices on stderr, so take the last line.
TARBALL=$(cd sdk/node && npm pack --ignore-scripts --pack-destination "$ROOT/dist" 2>/dev/null | tail -1)
[[ -n $TARBALL && -f dist/$TARBALL ]] || { echo "npm pack produced nothing"; exit 1; }
echo "  packed dist/$TARBALL"

echo "→ building sdk/python@$PY_VERSION"
(cd sdk/python && uv build --quiet --out-dir "$ROOT/dist")
# The wheel is named after [project].name, with '-' escaped to '_' (PEP 427)
# — not after the repo. Hard-coding it is how this script came to announce a
# file uv had never built and then fail at the cp, milestones later. The check
# is the point: if the name ever drifts again it fails here, immediately.
WHEEL="${PY_NAME//-/_}-$PY_VERSION-py3-none-any.whl"
[[ -f dist/$WHEEL ]] || { echo "uv build produced no dist/$WHEEL"; exit 1; }
echo "  built dist/$WHEEL"

# Default to the apps whose integration has actually landed. Vendoring into an
# app we have not integrated yet would leave an untracked artifact sitting in
# someone else's repo, which is not ours to do.
targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  targets=(app-node)   # M1; app-python joins with its own milestone
fi

for app in "${targets[@]}"; do
  dir="../$app"
  if [[ ! -d $dir ]]; then
    echo "· $app not checked out next to ozymandias — skipped"
    continue
  fi
  mkdir -p "$dir/vendor"
  case " $apps_node " in *" $app "*)
    # Remove older tarballs so the app's vendor dir never holds two versions
    # and package.json can only point at the one that is there.
    rm -f "$dir"/vendor/ozy-*.tgz
    cp "dist/$TARBALL" "$dir/vendor/"
    echo "✓ $app ← vendor/$TARBALL"
    echo "    package.json: \"ozy\": \"file:vendor/$TARBALL\"  (then npm install)"
    ;;
  esac
  case " $apps_python " in *" $app "*)
    rm -f "$dir"/vendor/ozy-*.whl
    cp "dist/$WHEEL" "$dir/vendor/"
    echo "✓ $app ← vendor/$WHEEL"
    echo "    pyproject: ozy @ file://\${PROJECT_ROOT}/vendor/$WHEEL  (then uv sync)"
    ;;
  esac
done
