#!/usr/bin/env bash
# Capture what *real* extended-StatsD clients put on the wire, into
# pkg/wire/testdata/statsd-compat/ (docs/plan/testing.md L7).
#
#   ./scripts/capture-statsd-compat.sh
#
# Run this by hand, not in CI: it downloads third-party clients. The goldens it
# writes are committed, so the test suite never needs the network or the
# clients themselves. Re-run it to refresh against newer client versions; if a
# capture changes, that is a compatibility signal worth reading, not a file to
# blindly re-commit.
#
# Why this exists: our parser's job is to accept what the ecosystem emits, and
# the ecosystem is the ground truth for that, not our reading of the spec. The
# two clients below are the ones an app would realistically reach for:
# datadogpy (Datadog's official Python client) and hot-shots (the de facto Node
# one). Both send things the spec's prose does not dwell on — a container-id
# field, `_e{}` events, `_sc` service checks — which is exactly what we want
# pinned.
#
# Everything sent here is synthetic. No capture may contain real hostnames,
# emails, tokens or user ids (AGENTS.md).
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=pkg/wire/testdata/statsd-compat
PORT=18125
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

command -v uv >/dev/null || { echo "need uv (https://docs.astral.sh/uv/)"; exit 1; }
command -v npm >/dev/null || { echo "need npm"; exit 1; }

# --- the listener ----------------------------------------------------------------
# Writes one line per datagram line, in arrival order. Datagram boundaries are
# preserved as a leading "# datagram" comment so the goldens show batching.
cat >"$WORK/listen.py" <<'PY'
import socket, sys, time
out = open(sys.argv[1], "w")
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("127.0.0.1", int(sys.argv[2])))
# Tell the shell the socket is bound. Sleeping "long enough" instead would
# lose every datagram on a slow start or a busy port, and the capture would
# overwrite a good golden with an empty file.
open(sys.argv[3], "w").close()
s.settimeout(0.5)
last = time.time()
n = 0
while time.time() - last < 3.0:
    try:
        data, _ = s.recvfrom(65535)
    except socket.timeout:
        continue
    last = time.time()
    n += 1
    text = data.decode("utf-8", "replace").rstrip("\n")
    print("# datagram %d (%d bytes, %d lines)" % (n, len(data), text.count("\n") + 1), file=out)
    print(text, file=out)
out.flush()
PY

capture() { # <name> <command...> — run the client with the listener up
  local name=$1; shift
  local ready="$WORK/$name.ready"
  rm -f "$ready"
  python3 "$WORK/listen.py" "$WORK/$name.txt" "$PORT" "$ready" &
  local lp=$!
  local waited=0
  while [[ ! -f $ready ]]; do
    sleep 0.2
    waited=$((waited + 1))
    if [[ $waited -gt 50 ]]; then
      kill $lp 2>/dev/null
      echo "listener never bound port $PORT — is something else using it?"
      exit 1
    fi
  done
  "$@"
  wait $lp
  local lines
  lines=$(grep -cv '^#' "$WORK/$name.txt" || true)
  # Never overwrite a committed golden with nothing. An empty capture means
  # the client or the listener failed, and the fixtures the compat tests read
  # are worth more than this run's output.
  if [[ ${lines:-0} -lt 1 ]]; then
    echo "captured 0 lines from $name — leaving $OUT/$name.txt untouched"
    exit 1
  fi
  {
    echo "# Captured by scripts/capture-statsd-compat.sh — do not hand-edit."
    echo "# client: $name"
    cat "$WORK/$name.txt"
  } >"$OUT/$name.txt"
  echo "wrote $OUT/$name.txt ($lines lines)"
}

# --- datadogpy -------------------------------------------------------------------
cat >"$WORK/emit.py" <<'PY'
import os, random
from datadog.dogstatsd import DogStatsd

# Sample rates are applied client-side by rolling a die, so an unseeded run
# would emit app.sampled only about half the time and the golden would churn.
# Seed 1's first roll is below 0.5, so the line is always sent.
random.seed(1)

# A synthetic container id: a real one identifies a real machine. The `|c:`
# field it produces is exactly the kind of extension our parser must ignore
# rather than choke on.
CONTAINER = "0" * 64

# disable_buffering=False would batch everything into one datagram; capture
# both shapes, since our server has to handle each.
d = DogStatsd(host="127.0.0.1", port=int(os.environ["PORT"]), disable_buffering=True,
              constant_tags=["env:test"], container_id=CONTAINER,
              origin_detection_enabled=False)
d.increment("app.requests", tags=["route:/api/items", "method:get"])
d.increment("app.requests", value=7)
d.decrement("app.queue.depth")
d.gauge("app.workers", 4.5)
d.histogram("app.payload.bytes", 2048)
d.timing("app.request.duration", 12.5)
d.distribution("app.latency", 0.25)
d.set("app.users", "user-1")
d.increment("app.sampled", sample_rate=0.5)
d.gauge("app.unicode.tag", 1, tags=["team:ops"])
# |T: a client-supplied timestamp, for backfilled points.
d.count_with_timestamp("app.backfilled", 3, timestamp=1700000000)
d.gauge_with_timestamp("app.backfilled.level", 9, timestamp=1700000000)
# |card: a field newer than this parser. Unknown fields must be ignored, not
# rejected — that is the whole reason the format is extensible.
d.increment("app.cardinality", cardinality=DogStatsd.CARDINALITY_HIGH)
d.event("deploy", "shipped v1", alert_type="info", tags=["service:web"])
d.service_check("app.can_connect", 0, tags=["service:web"])
d.flush()

b = DogStatsd(host="127.0.0.1", port=int(os.environ["PORT"]), disable_buffering=False)
for i in range(4):
    b.increment("app.batched", tags=["i:%d" % i])
b.flush()
PY
PORT=$PORT capture datadogpy env PORT="$PORT" uv run --quiet --with datadog python "$WORK/emit.py"

# --- hot-shots -------------------------------------------------------------------
mkdir -p "$WORK/node"
cat >"$WORK/node/package.json" <<'JSON'
{"name": "capture", "private": true, "type": "commonjs", "dependencies": {"hot-shots": "^10.0.0"}}
JSON
cat >"$WORK/node/emit.js" <<'JS'
// Same reason as the seed in emit.py: hot-shots rolls Math.random() for
// sample rates, so an unpatched run emits app.sampled only half the time.
Math.random = () => 0.1;
const StatsD = require("hot-shots");
const c = new StatsD({ host: "127.0.0.1", port: Number(process.env.PORT), globalTags: ["env:test"] });
c.increment("app.requests", 1, ["route:/api/items", "method:get"]);
c.increment("app.requests", 7);
c.decrement("app.queue.depth");
c.gauge("app.workers", 4.5);
c.histogram("app.payload.bytes", 2048);
c.timing("app.request.duration", 12.5);
c.distribution("app.latency", 0.25);
c.set("app.users", "user-1");
c.increment("app.sampled", 1, 0.5);
c.event("deploy", "shipped v1", { alert_type: "info" }, ["service:web"]);
c.check("app.can_connect", 0, null, ["service:web"]);
// maxBufferSize batches; flush at the end so the datagram is captured.
const b = new StatsD({ host: "127.0.0.1", port: Number(process.env.PORT), maxBufferSize: 512 });
for (let i = 0; i < 4; i++) b.increment("app.batched", 1, [`i:${i}`]);
setTimeout(() => { b.close(() => c.close(() => {})); }, 300);
JS
(cd "$WORK/node" && npm install --silent --no-audit --no-fund >/dev/null)
PORT=$PORT capture hot-shots env PORT="$PORT" node "$WORK/node/emit.js"

# --- versions, so a golden can be traced back ------------------------------------
{
  echo "# Client versions used for the captures in this directory."
  echo "# Regenerate with scripts/capture-statsd-compat.sh."
  echo "datadogpy $(uv run --quiet --with datadog python -c 'import datadog; print(datadog.__version__)')"
  echo "hot-shots $(cd "$WORK/node" && node -p 'require("hot-shots/package.json").version')"
  echo "captured   $(date -u +%Y-%m-%d)"
} >"$OUT/VERSIONS.txt"
cat "$OUT/VERSIONS.txt"
