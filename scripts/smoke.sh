#!/usr/bin/env bash
# End-to-end smoke test against the running compose stack (docs/plan/testing.md
# L9). It grows with every milestone; each block below is labelled with the
# milestone that added it. Must stay under two minutes.
#
# Usage: make up && make smoke
#   OZY_URL (default http://localhost:9400)
#   AGENT_URL     (default http://localhost:8126)
set -euo pipefail
cd "$(dirname "$0")/.."

OZY_URL=${OZY_URL:-http://localhost:9400}
AGENT_URL=${AGENT_URL:-http://localhost:8126}
COMPOSE=(docker compose -f deploy/docker-compose.yml)

pass=0
check() { # <description> <command...>
  local desc=$1; shift
  # stderr is kept for the failure report: the wait_* helpers say there what
  # they saw, which is usually the one line that explains the failure.
  local err; err=$(mktemp)
  if "$@" >/dev/null 2>"$err"; then
    echo "✓ $desc"; pass=$((pass + 1)); rm -f "$err"
  else
    echo "✗ $desc"
    echo "  command: $*"
    sed 's/^/  /' "$err"; rm -f "$err"
    echo "  --- recent logs ---"
    "${COMPOSE[@]}" logs --tail 20 2>&1 | sed 's/^/  /'
    exit 1
  fi
}
body_has() { curl -fsS --max-time 5 "$1" | grep -q -- "$2"; }
header_has() { curl -fsS --max-time 5 -o /dev/null -D - "$1" | tr -d '\r' | grep -qi -- "$2"; }
healthy() { [[ $(docker inspect -f '{{.State.Health.Status}}' "$("${COMPOSE[@]}" ps -q "$1")") == healthy ]]; }

# --- M0: both processes up, healthy, serving -----------------------------------
check "ozyd /healthz"                 body_has "$OZY_URL/healthz" '"component":"ozyd"'
check "agent /healthz"                     body_has "$AGENT_URL/healthz" '"component":"agent"'
check "ozyd container healthy"        healthy ozyd
check "agent container healthy"            healthy agent
check "agent tags data with the Mac's name" body_has "$AGENT_URL/healthz" "\"hostname\":\"$(scripts/hostname.sh)\""
check "agent forwards to ozyd by name" body_has "$AGENT_URL/healthz" '"intake_url":"http://ozyd:9400"'
check "ozyd self-metrics"             body_has "$OZY_URL/debug/vars" 'ozy.build.info'
check "UI index served"                    body_has "$OZY_URL/" '<div id="root">'
check "UI deep link falls back to index"   body_has "$OZY_URL/logs/live" '<div id="root">'
# The dashboard routes are client-side, so a deep link only works if the server
# serves the index for a path it has no file for. Both shapes, because the
# service one has a segment the router matches ahead of ":id".
check "a dashboard deep link is served"    body_has "$OZY_URL/dashboards/1" '<div id="root">'
check "a service dashboard link is served" body_has "$OZY_URL/dashboards/service/api" '<div id="root">'
check "UI index is not cached"             header_has "$OZY_URL/" 'cache-control: no-cache'

# M0 acceptance: SIGTERM → clean exit (0) within the grace period.
for svc in agent ozyd; do
  cid=$("${COMPOSE[@]}" ps -q "$svc")
  start=$(date +%s)
  docker kill -s TERM "$cid" >/dev/null
  docker wait "$cid" >/tmp/ozymandias-smoke-exit
  took=$(($(date +%s) - start))
  check "$svc exits 0 on SIGTERM (${took}s)" test "$(cat /tmp/ozymandias-smoke-exit)" = 0 -a "$took" -le 10
done
# Restart the same containers (not `up`, which could rebuild or recreate).
"${COMPOSE[@]}" start >/dev/null 2>&1
for svc in ozyd agent; do
  for _ in $(seq 1 30); do healthy "$svc" && break; sleep 1; done
  check "$svc healthy again after restart" healthy "$svc"
done

# --- M1: statsd in, query out --------------------------------------------------
# Every send below runs *inside* a container on the ozymandias network, not from
# the Mac: Colima's ssh port forwarder drops UDP, so `nc` on the host would
# silently send into a void (docs/operations.md). busybox is the smallest image
# with an `nc` that speaks UDP.
#
# `-w1`, not `-w0`: busybox reads -w0 as "wait forever" (BSD nc reads it as
# "don't wait"), so -w0 hangs the container. -w1 costs a second per datagram,
# which is why sends are batched into as few containers as possible.
statsd() { # <line>... — all lines in ONE datagram (statsd is newline-framed)
  printf '%s\n' "$@" | docker run --rm -i --network ozymandias busybox nc -u -w1 agent 8125
}
statsd_burst() { # <datagrams> <lines each> <line> — separate datagrams, one container
  docker run --rm --network ozymandias -e D="$1" -e L="$2" -e LINE="$3" busybox sh -c '
    i=0
    while [ "$i" -lt "$D" ]; do
      j=0
      while [ "$j" -lt "$L" ]; do printf "%s\n" "$LINE"; j=$((j + 1)); done |
        nc -u -w1 agent 8125
      i=$((i + 1))
    done'
}
# query_sum <metric> — Σ since this run started, "null" if the metric is unknown.
# Scoped to the run, not to a fixed window: two smoke runs in a row would
# otherwise sum to 50 and fail. One interval of slack, because the bucket that
# holds the first sample starts before we do.
# ozyd_now is ozyd's clock (its Date header): the VM's, which stamps every
# point and can drift from the Mac's while the Mac sleeps — by hours, until
# the VM steps it back. Query windows use it, not `date`, or a skewed VM
# puts this run's points outside them. now_s falls back to the Mac's clock
# when ozyd does not answer.
ozyd_now() {
  curl -fsS --max-time 2 -o /dev/null -D - "$OZY_URL/healthz" | tr -d '\r' |
    python3 -c 'import sys, email.utils
for line in sys.stdin:
    if line.lower().startswith("date:"):
        print(int(email.utils.parsedate_to_datetime(line[5:].strip()).timestamp()))'
}
now_s() {
  local n
  n=$(ozyd_now 2>/dev/null)
  if [[ -n $n ]]; then echo "$n"; else date +%s; fi
}
query_sum() {
  curl -fsS --max-time 5 "$OZY_URL/api/v1/query?metric=$1&agg=sum&from=$((M1_T0 - 10))&to=$(now_s)" |
    python3 -c 'import json,sys
d = json.load(sys.stdin)
pts = [p[1] for s in d.get("series", []) for p in s["points"] if p[1] is not None]
print(int(sum(pts)) if pts else "null")'
}
# query_agg <metric> <agg> — the last non-null point of an aggregation over
# this run's window, or "null". Used for percentiles, where summing is
# meaningless: what matters is the value itself.
query_agg() {
  curl -fsS --max-time 5 "$OZY_URL/api/v1/query?metric=$1&agg=$2&from=$((M1_T0 - 10))&to=$(now_s)" |
    python3 -c 'import json,sys
d = json.load(sys.stdin)
pts = [p[1] for s in d.get("series", []) for p in s["points"] if p[1] is not None]
print(pts[-1] if pts else "null")'
}
# within <value> <want> <tolerance-fraction> — the sketch guarantee, checked
# rather than assumed.
within() {
  python3 -c 'import sys
got, want, tol = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
if got == "null": sys.exit(1)
sys.exit(0 if abs(float(got) - want) <= tol * want else 1)' "$1" "$2" "$3"
}
# wait_agg <metric> <agg> <want> <tolerance> — poll until the flush lands and
# the answer is within tolerance.
wait_agg() {
  for _ in $(seq 1 30); do
    within "$(query_agg "$1" "$2")" "$3" "$4" && return 0
    sleep 1
  done
  echo "$2:$1 = $(query_agg "$1" "$2"), expected $3 ±$4" >&2
  return 1
}
# wait_sum <metric> <expected> — poll until the agent flushes (10s) and the
# intake stores it. Generous: a cold SQLite write on a loaded laptop is slow.
wait_sum() {
  for _ in $(seq 1 30); do
    [[ "$(query_sum "$1")" == "$2" ]] && return 0
    sleep 1
  done
  echo "$1: Σ = $(query_sum "$1"), expected $2" >&2
  return 1
}

# Send everything first, then assert: one 10s flush covers all of it, so the
# whole M1 block costs one flush rather than one per check.
M1_T0=$(now_s)
N=25
statsd_burst 5 5 'smoke.test:1|c|#source:smoke'          # 5 datagrams × 5 lines
statsd 'smoke.gauge:1|g' 'smoke.gauge:2|g' 'smoke.gauge:7|g'
# A distribution, sent as a hundred distinct values so the percentiles have
# something to be wrong about: p50 is 50, p95 is 95, p99 is 99.
statsd $(i=1; while [ $i -le 100 ]; do printf "smoke.latency:%d|d|#source:smoke " "$i"; i=$((i + 1)); done)
# The two metrics the shipped template dashboard is built on, tagged with a
# service so that template instantiation has something to discover. Sent here so
# they ride the same flush as everything else; asserted in the M3 block.
SMOKE_SVC=smoke-svc
statsd "http.request.count:1|c|#service:$SMOKE_SVC,route:/items,status:200" \
       "http.request.duration:12|d|#service:$SMOKE_SVC,route:/items"
# The protocol is the interface: no SDK, no library, just a shell script and nc.
check "examples/cron-script.sh runs"       docker run --rm --network ozymandias \
  -e OZY_AGENT_HOST=agent -e JOB=smoke \
  -v "$PWD/examples:/examples:ro" busybox sh /examples/cron-script.sh

check "statsd counter queries back (Σ=$N)" wait_sum smoke.test "$N"
# A gauge is last-write-wins within a flush, not a sum: three values, one point.
check "gauge keeps the last value"         wait_sum smoke.gauge 7
check "the cron script's counter landed"   wait_sum cron.job.runs 1
check "metric appears in the catalogue"    body_has "$OZY_URL/api/v1/metrics?prefix=smoke" '"smoke.test"'
check "its tag key is listed"              body_has "$OZY_URL/api/v1/tags?metric=smoke.test" '"source"'
check "its tag value is listed"            body_has "$OZY_URL/api/v1/tags/values?metric=smoke.test&key=source" '"smoke"'
check "its series are counted"             body_has "$OZY_URL/api/v1/metrics/cardinality?prefix=smoke.test" '"name":"smoke.test"'
check "its tag keys are counted"           body_has "$OZY_URL/api/v1/tags/cardinality?metric=smoke.test" '"key":"source"'
# ozyd's self-metrics carry host:<OZY_HOSTNAME>. Unset in a container, that
# would be the container id, and every `make up` would add a host value (and a
# copy of every self-metric series) to the store. The volume keeps old values,
# so listing tag values proves nothing; ask which hosts ozyd reported from in
# the last 25s, which must be the Mac's name alone.
#
# "Now" is ozyd's, from its Date header, not the Mac's: ozyd stamps points
# with the VM's clock, which can drift from the Mac's after a sleep, and
# neither "the newest bucket" nor a wide window survives the VM clock being
# corrected backwards (old points would then be the newest for a while).
# Right after a recreate the old container's last bucket is still in the
# window; the retries outlast it. Tag values are lower-cased on the wire.
# (ozyd_now is defined with query_sum, near the top.)
recent_ozyd_hosts() {
  local now body
  now=$(ozyd_now 2>/dev/null) && [[ -n $now ]] || { echo "(ozyd unreachable)"; return; }
  body=$(curl -fsS --max-time 2 -G "$OZY_URL/api/v1/query" \
    --data-urlencode 'q=max:ozy.build.info{component:ozyd} by {host}' \
    --data-urlencode "from=$((now - 25))" --data-urlencode "to=$now" 2>/dev/null) ||
    { echo "(query failed)"; return; }
  # A group without the by-key has no "host" in its tags (docs/api.md).
  printf '%s' "$body" | python3 -c 'import json,sys
series = json.load(sys.stdin).get("series", [])
print(",".join(sorted({s["tags"].get("host", "<none>") for s in series
                       if any(p[1] is not None for p in s["points"])})))'
}
ozyd_host_is_the_macs() {
  local want; want=$(printf '%s' "$(scripts/hostname.sh)" | tr '[:upper:]' '[:lower:]')
  for _ in $(seq 1 25); do
    [[ "$(recent_ozyd_hosts)" == "$want" ]] && return 0
    sleep 1
  done
  echo "ozyd's self-metrics in the last 25s are from host(s) '$(recent_ozyd_hosts)', expected '$want'" >&2
  return 1
}
check "ozyd tags its own metrics with the Mac's name" ozyd_host_is_the_macs
check "the histogram became percentiles"   body_has "$OZY_URL/api/v1/metrics?prefix=cron.job.duration" '"cron.job.duration.95percentile"'

# A distribution is the other half of M2: the sketch goes to Pebble whole and
# its four exact aggregates go to the TSDB as ordinary series. Both are
# checked, because a p95 that is right while the count is wrong means the two
# stores have drifted apart.
check "p50 of the distribution"            wait_agg smoke.latency p50 50 0.01
check "p95 of the distribution"            wait_agg smoke.latency p95 95 0.01
check "p99 of the distribution"            wait_agg smoke.latency p99 99 0.01
check "its count is exact"                 wait_sum smoke.latency.count 100
check "its sum is exact"                   wait_sum smoke.latency.sum 5050
check "its max is exact"                   test "$(query_agg smoke.latency.max max)" = "100"
check "a distribution refuses avg"         test "$(curl -s -o /dev/null -w '%{http_code}' \
  "$OZY_URL/api/v1/query?metric=smoke.latency&agg=avg")" = "400"
check "the agent counts what it parsed"    body_has "$AGENT_URL/debug/vars" 'ozy.agent.statsd.messages_received'

# --- M2: the real TSDB ---------------------------------------------------------
# The engine is meant to be invisible from out here — M1's checks above ran
# against it already. What is left to prove is that it is the engine actually
# running, that it reports what it refused, and that a block cut and a restart
# do not change an answer.
gauge_value() { # <metric> <tag> — an ozyd self-metric from /debug/vars
  curl -fsS --max-time 5 "$OZY_URL/debug/vars" |
    tr '{' '\n' | grep -F "\"$1\"" | grep -F "$2" | sed 's/.*"value"://; s/[^0-9.-].*//'
}

check "the tsdb is the store in use"       body_has "$OZY_URL/debug/vars" '"store:tsdb"'
check "it reports its head size"           body_has "$OZY_URL/debug/vars" 'ozy.tsdb.head.series'
check "it reports what it refused"         body_has "$OZY_URL/debug/vars" 'ozy.tsdb.ooo_rejected'
# Nothing above sends out-of-order data, so a non-zero count here means the
# store is dropping samples nobody asked it to drop.
check "no samples were dropped"            test "$(gauge_value ozy.tsdb.ooo_rejected store:tsdb)" = "0"
check "no series hit the cardinality cap"  test "$(gauge_value ozy.tsdb.series_limit_rejected store:tsdb)" = "0"
check "the store reports its disk use"     test "$(gauge_value ozy.tsdb.disk_bytes store:tsdb)" -gt 0
check "the sketch store reports its size"  test "$(gauge_value ozy.sketchstore.series '')" -gt 0
# A collision means two metrics would have shared a percentile, and one of
# them is being refused. It should be zero forever.
check "no series hashed to the same id"    test "$(gauge_value ozy.sketchstore.id_collisions '')" = "0"

# --- M3: the query language ----------------------------------------------------
# The endpoint now evaluates metricql, so what is worth proving out here is the
# part the M1 parameters could not express at all, and that the two spellings
# reach the same engine.

# query_expr <expr> [extra curl args…] — the last non-null point of the first
# line of a metricql query, or "null".
query_expr() {
  local expr=$1
  shift
  curl -fsS --max-time 5 --get "$OZY_URL/api/v1/query" \
    --data-urlencode "q=$expr" -d "from=$((M1_T0 - 10))" -d "to=$(now_s)" "$@" |
    python3 -c 'import json,sys
d = json.load(sys.stdin)
pts = [p[1] for s in d.get("series", []) for p in s["points"] if p[1] is not None]
print(pts[-1] if pts else "null")'
}
# field <url> <json path…> — one value out of a response, as text.
field() {
  local url=$1
  shift
  curl -fsS --max-time 5 "$url" | python3 -c 'import json,sys
d = json.load(sys.stdin)
for k in sys.argv[1:]:
    d = d[int(k)] if isinstance(d, list) else d[k]
print(d)' "$@"
}

# A ratio of two queries is the shape M1 had no room for: two selections in
# one request. A metric over itself is exactly 1 whatever the data did.
check "a ratio of two queries"             test "$(query_expr \
  'sum:smoke.latency.count{*} / sum:smoke.latency.count{*}')" = "1"
# A template variable is bound by the caller, not written into the query.
check "a template variable binds"          test "$(query_expr \
  'sum:smoke.latency.count{$scope}' --data-urlencode 'var.scope=host:*')" = "100"
# The response says what it evaluated, canonically spelled — which is what a
# dashboard stores and what somebody migrating off the M1 parameters copies.
check "the response echoes the query"      test "$(field \
  "$OZY_URL/api/v1/query?metric=smoke.latency.count&agg=sum&by=host&from=$((M1_T0 - 10))&to=$(now_s)" \
  query)" = "sum:smoke.latency.count{*} by {host}"
# And names each line the same way for every client.
check "each line carries its scope"        test "$(field \
  "$OZY_URL/api/v1/query?metric=smoke.latency.count&agg=sum&from=$((M1_T0 - 10))&to=$(now_s)" \
  series 0 scope)" = "*"
# POST takes the same query, because a dashboard's outgrows a URL.
# post_query <json body> — Σ of every non-null point, as an integer.
post_query() {
  curl -fsS --max-time 5 "$OZY_URL/api/v1/query" -d "$1" |
    python3 -c 'import json,sys
d = json.load(sys.stdin)
print(int(sum(p[1] for s in d["series"] for p in s["points"] if p[1] is not None)))'
}
# POST takes the same query as a body, because a dashboard's outgrows a URL.
# The body is built here and not inside the `$(…)` below: a `\"` written inside
# a command substitution inside a quoted argument reaches curl as a literal
# backslash, and the server sees JSON that is not the JSON written here.
post_body="{\"q\": \"sum:smoke.latency.count{*}\", \"from\": $((M1_T0 - 10)), \"to\": $(now_s)}"
check "POST takes a query body"            test "$(post_query "$post_body")" = "100"

# The editor endpoint answers 200 with a column to underline, not an exception.
validate_col() { # <query> — "<ok> <col>" for a query that does not parse
  curl -fsS --max-time 5 "$OZY_URL/api/v1/query/validate" --data-binary @- <<<"{\"q\": \"$1\"}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["ok"], d["error"]["col"])'
}
broken_col=$(validate_col 'sum:x{a:b by {k}')
check "a broken query reports its column"  test "$broken_col" = "False 14"

# A dashboard whose template variable is cleared sends the parameter empty. It
# has to mean "all" — binding "" would refuse the request, which is how a
# dashboard goes blank the moment somebody clears a selector.
check "a cleared variable means all"       test "$(query_expr \
  'sum:smoke.latency.count{$scope}' --data-urlencode 'var.scope=')" = "100"

# --- M3: one dashboard, one request --------------------------------------------
# The batch endpoint is what a dashboard actually calls. Three things here that
# no unit test sees: the real store behind the shared selection, a request whose
# body is built by a shell the way a client's would be, and the shape a client
# decodes — in particular that one broken query does not take the others down.
#
# The body is built in a variable first, for the reason spelled out above the
# single-query POST: a `\"` inside a command substitution reaches curl as a
# literal backslash.
batch_body="{\"queries\": ["
batch_body="$batch_body {\"q\": \"sum:smoke.latency.count{*}\"},"
batch_body="$batch_body {\"q\": \"avg:smoke.latency.count{*} by {host}\"},"
batch_body="$batch_body {\"q\": \"sum:smoke.latency.count{\"}"
batch_body="$batch_body ], \"from\": $((M1_T0 - 10)), \"to\": $(now_s)}"
# batch_field <expr> — a python expression over the decoded response, as `d`.
batch_field() {
  curl -fsS --max-time 10 "$OZY_URL/api/v1/query/batch" -d "$batch_body" |
    python3 -c "import json,sys
d = json.load(sys.stdin)
print($1)"
}
check "a batch answers every query"        test "$(batch_field 'len(d["results"])')" = "3"
check "a batch keeps the request order"    test "$(batch_field '[r["index"] for r in d["results"]]')" \
  = "[0, 1, 2]"
check "the good queries answered"          test "$(batch_field \
  'sum(1 for r in d["results"] if r["status"] == "ok")')" = "2"
# The whole point: one unparseable query is that query's failure and nobody
# else's. A 400 for the batch here would mean a dashboard goes blank on a typo.
check "one bad query is not a bad batch"   test "$(batch_field 'd["status"]')" = "ok"
check "the bad query reports its own code" test "$(batch_field 'd["results"][2]["code"]')" = "400"
# series and warnings are always there, on a failure as much as on a success:
# a client must be able to tell "nothing to draw" from "field not in your version".
check "every result has the same shape"    test "$(batch_field \
  'all("series" in r and "warnings" in r for r in d["results"])')" = "True"
# The batch's numbers agree with asking one at a time, which is what the shared
# selection must not change.
check "a batch agrees with a single query" test "$(batch_field \
  'int(sum(p[1] for p in d["results"][0]["series"][0]["points"] if p[1] is not None))')" = "100"
# Each result says which grid it was drawn on. Per result rather than per batch,
# because a `.rollup()` sets the grid of the query it is written on — so a client
# drawing a shared crosshair has to read it from the widget, not from the batch.
check "each result reports its grid"       test "$(batch_field \
  'all(r["interval"] > 0 for r in d["results"] if r["status"] == "ok")')" = "True"
check "the batch does not claim one grid"  test "$(batch_field '"interval" in d')" = "False"

# --- M3: the distribution endpoint ---------------------------------------------
# The heatmap's data. Three things no unit test covers: the real sketch store
# behind it (a sketch is filed under the bare metric while selection runs against
# `<metric>.count` — get that wrong and you get an empty answer, not an error),
# the real agent's sketches rather than a fixture's, and the shape a client
# decodes.
sketch_field() { # <python expression over the decoded response, as `d`>
  curl -fsS --max-time 10 \
    "$OZY_URL/api/v1/query/sketch?q=dist:smoke.latency%7B*%7D&from=$((M1_T0 - 10))&to=$(now_s)" |
    python3 -c "import json,sys
d = json.load(sys.stdin)
print($1)"
}
check "a distribution has a shape"         test "$(sketch_field 'len(d["series"]) > 0')" = "True"
# Non-vacuity, first: every check below is an `all(...)` over the bins, and
# `all()` of nothing is True. Without this line an empty response would pass the
# whole section.
check "it has bins to draw"                test "$(sketch_field 'd["bins"] > 0')" = "True"
check "its bins are [lower, upper, count]" test "$(sketch_field \
  'all(len(b) == 3 for s in d["series"] for k in s["buckets"] for b in k["bins"])')" = "True"
# Ascending, and each bin's own bounds the right way round. A mirrored axis
# draws a plausible histogram, which is the failure worth catching.
check "its bins ascend by value"           test "$(sketch_field \
  'all(b[0] <= b[1] for s in d["series"] for k in s["buckets"] for b in k["bins"])
   and all(k["bins"] == sorted(k["bins"]) for s in d["series"] for k in s["buckets"])')" = "True"
# The aggregates are carried, not estimated: every observation is in some bin.
check "its bins hold every observation"    test "$(sketch_field \
  'all(abs(sum(b[2] for b in k["bins"]) - k["count"]) < 1e-9 for s in d["series"] for k in s["buckets"])')" = "True"
# Per bucket, not per response: two groups can carry different relative
# accuracies, so one number for the whole answer described the first and was
# applied to the rest.
check "each bucket reports its error bar"  test "$(sketch_field \
  'all(k["gamma"] > 1 for s in d["series"] for k in s["buckets"])')" = "True"
check "the response claims no one gamma"   test "$(sketch_field '"gamma" in d')" = "False"
# The two endpoints know where the other one is — a 400 either way, with the
# other endpoint named, rather than an empty 200.
sketch_err() { # <query> <endpoint> — the error message
  curl -sS --max-time 5 "$OZY_URL/api/v1/$2?q=$1&from=$((M1_T0 - 10))&to=$(now_s)" |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("error", ""))'
}
check "a number is sent to /query"         test -n "$(sketch_err 'p95:smoke.latency%7B*%7D' 'query/sketch' | grep 'api/v1/query')"
check "a distribution is sent to /sketch"  test -n "$(sketch_err 'dist:smoke.latency%7B*%7D' 'query' | grep 'api/v1/query/sketch')"
# POST with variables is the verb the heatmap widget uses, and nothing above
# covers it: a dashboard's `dist:` query carries `$service`/`$env` and the
# bindings are an object, which is why it is not a query string.
# Built in a variable first — see the note above the single-query POST.
sketch_post_body="{\"q\": \"dist:smoke.latency{\$scope}\", \"from\": $((M1_T0 - 10)), \"to\": $(now_s), \"vars\": {\"scope\": []}}"
sketch_post_field() { # <python expression over the decoded response, as `d`>
  curl -fsS --max-time 10 "$OZY_URL/api/v1/query/sketch" -d "$sketch_post_body" |
    python3 -c "import json,sys
d = json.load(sys.stdin)
print($1)"
}
check "POST /sketch takes bound variables" test "$(sketch_post_field 'len(d["series"]) > 0')" = "True"
check "and answers with an interval"       test "$(sketch_post_field 'd["interval"] > 0')" = "True"

# --- M3: dashboards ------------------------------------------------------------
# Provisioning happens at startup, inside the container, from a directory baked
# into the image. That is three things the unit tests cannot check: that the
# directory is actually in the image, that OZY_PROVISIONING_PATHS points at it,
# and that a definition written by hand passes the validator running in the
# binary that shipped.

# json_field <url> <python expression over `d`> — one value, as text.
json_field() {
  curl -fsS --max-time 5 "$1" | python3 -c "import json,sys
d = json.load(sys.stdin)
print($2)"
}

check "the home dashboard was provisioned" \
  test "$(json_field "$OZY_URL/api/v1/dashboards" \
    '[x["uid"] for x in d["dashboards"] if x.get("uid")=="home"][0]')" = "home"
# Provisioned means read-only over HTTP: the next restart would undo an edit.
check "a provisioned dashboard refuses a write" \
  test "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
    "$OZY_URL/api/v1/dashboards/$(json_field "$OZY_URL/api/v1/dashboards" \
      '[x["id"] for x in d["dashboards"] if x.get("uid")=="home"][0]')")" = "409"
# The definition comes back with its widgets, not as an opaque blob.
check "it comes back with its widgets" \
  test "$(json_field "$OZY_URL/api/v1/dashboards" \
    'len([x for x in d["dashboards"] if x.get("uid")=="home"][0]["widgets"]) > 3')" = "True"

# CRUD, over the wire, against the real database.
dash_body='{"title":"smoke","widgets":[{"id":"w1","type":"timeseries","layout":{"x":0,"y":0,"w":6,"h":3},"queries":[{"q":"sum:smoke.test{*}","display":"line"}]}]}'
created=$(curl -fsS --max-time 5 -X POST "$OZY_URL/api/v1/dashboards" -d "$dash_body" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
check "a dashboard can be created"          test -n "$created"
check "and read back"                       test "$(json_field \
  "$OZY_URL/api/v1/dashboards/$created" 'd["title"]')" = "smoke"
check "and deleted"                         test "$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$OZY_URL/api/v1/dashboards/$created")" = "204"
check "and is then gone"                    test "$(curl -s -o /dev/null -w '%{http_code}' \
  "$OZY_URL/api/v1/dashboards/$created")" = "404"
# A definition whose query does not parse must not be storable: the validator
# has to be the one in the shipped binary, not just the one in the tests.
check "a broken definition is refused"      test "$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$OZY_URL/api/v1/dashboards" \
  -d '{"title":"bad","widgets":[{"id":"w","type":"timeseries","layout":{"x":0,"y":0,"w":1,"h":1},"queries":[{"q":"sum:x{a:b by {k}","display":"line"}]}]}')" = "400"
# A field on a type that does not draw it is refused, not ignored: the editor
# lists such fields as "not used by this type" on the strength of this.
check "a chart's conditional format is refused" test "$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$OZY_URL/api/v1/dashboards" \
  -d '{"title":"bad","widgets":[{"id":"w","type":"timeseries","layout":{"x":0,"y":0,"w":1,"h":1},"queries":[{"q":"sum:x{*}"}],"conditional_formats":[{"op":">","value":1,"color":"red"}]}]}')" = "400"
# The editor's query box asks this on every pause in typing.
check "validate names the column of a parse error" test "$(curl -fsS --max-time 5 \
  -X POST "$OZY_URL/api/v1/query/validate" -d '{"q":"sum:x{a:b by {k}"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["error"]["col"])')" = "14"

# --- M3: dashboard templates ---------------------------------------------------
# A template is the one dashboard nobody writes: it is provisioned once and
# served per service. Three things only an end-to-end check covers — that the
# shipped template is in the image and validates in the binary that shipped,
# that service discovery finds a service through the real tag index (including a
# distribution, whose tags live on its `.count`), and that instantiation binds
# the variable rather than rewriting the query.

# wait_service <name> — poll until discovery sees it. The metrics it is
# discovered from arrive on the agent's flush like everything else.
wait_service() {
  for _ in $(seq 1 30); do
    [[ "$(json_field "$OZY_URL/api/v1/dashboards/services" "'$1' in d[\"services\"]")" == "True" ]] && return 0
    sleep 1
  done
  echo "$1 is not in $(curl -fsS --max-time 5 "$OZY_URL/api/v1/dashboards/services")" >&2
  return 1
}
check "the service template was provisioned" \
  test "$(json_field "$OZY_URL/api/v1/dashboards" \
    '[x["uid"] for x in d["dashboards"] if x.get("uid")=="service"][0]')" = "service"
# A template is not shown as itself, so it says so in the definition the list
# serves.
check "and is marked as a template" \
  test "$(json_field "$OZY_URL/api/v1/dashboards" \
    '[x.get("template") for x in d["dashboards"] if x.get("uid")=="service"][0]')" = "True"
check "the service is discovered from the template's metrics" wait_service "$SMOKE_SVC"
# Discovery is not "every tag value in the store": nothing sent a service tag on
# smoke.test, and the template does not query it.
check "and nothing else is" \
  test "$(json_field "$OZY_URL/api/v1/dashboards/services" \
    'len(d["services"])')" = "1"
check "the list is not truncated" \
  test "$(json_field "$OZY_URL/api/v1/dashboards/services" 'd["truncated"]')" = "False"
check "and no template row is unreadable" \
  test "$(json_field "$OZY_URL/api/v1/dashboards/services" 'len(d["unreadable"])')" = "0"
# The instance: one per template, titled for the service, with the variable bound
# and the query text untouched.
svc_url="$OZY_URL/api/v1/dashboards/service/$SMOKE_SVC"
check "the template instantiates for it" \
  test "$(json_field "$svc_url" 'd["dashboards"][0]["template_uid"]')" = "service"
check "the instance names the service" \
  test "$(json_field "$svc_url" "'$SMOKE_SVC' in d[\"dashboards\"][0][\"dashboard\"][\"title\"]")" = "True"
check "the service variable is bound" \
  test "$(json_field "$svc_url" \
    '[v["default"] for v in d["dashboards"][0]["dashboard"]["template_vars"] if v["name"]=="service"][0]')" = "$SMOKE_SVC"
check "the instance is not itself a template" \
  test "$(json_field "$svc_url" '"template" in d["dashboards"][0]["dashboard"]')" = "False"
# Bound, not rewritten: the evaluator resolves $service per request.
check "the queries still say \$service" \
  test "$(json_field "$svc_url" \
    'all("$service" in q["q"] for w in d["dashboards"][0]["dashboard"]["widgets"] for q in w.get("queries", []))')" = "True"
# An unknown service is a 404, not a grid of empty charts.
check "an unknown service is refused" \
  test "$(curl -s -o /dev/null -w '%{http_code}' \
    "$OZY_URL/api/v1/dashboards/service/not-a-service")" = "404"
# And the instance is a definition the API would accept, which is what makes
# "save a copy of this" possible.
copy=$(curl -fsS --max-time 5 "$svc_url" |
  python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["dashboards"][0]["dashboard"]))')
copied=$(curl -fsS --max-time 5 -X POST "$OZY_URL/api/v1/dashboards" -d "$copy" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
check "an instance can be saved as a dashboard" test -n "$copied"
# Deleted again: a smoke run should not leave rows behind for the next one.
curl -fsS --max-time 5 -o /dev/null -X DELETE "$OZY_URL/api/v1/dashboards/$copied"

# The durability claim, end to end. The SIGTERM cycle near the top of this
# script happened before any of this data existed, so repeating a query after
# it proved nothing — which is what this check used to do. Kill ozyd
# *now*, with these samples in the head and durable only in the write-ahead
# log, and ask again on the other side. This is the only end-to-end exercise
# of replay there is, and replay is where every serious bug in M2 was.
cid=$("${COMPOSE[@]}" ps -q ozyd)
docker kill -s TERM "$cid" >/dev/null
docker wait "$cid" >/dev/null
"${COMPOSE[@]}" start ozyd >/dev/null 2>&1
for _ in $(seq 1 30); do healthy ozyd && break; sleep 1; done
check "ozyd healthy after the durability restart" healthy ozyd
check "the counter survived a real restart"  wait_sum smoke.test "$N"
check "the gauge survived a real restart"    wait_sum smoke.gauge 7
check "the sketch survived a real restart"  wait_agg smoke.latency p95 95 0.01

# --- M3: agent collectors ------------------------------------------------------
# The host collector reads the Docker VM's kernel (not the Mac's) every 15s,
# after a random start within its first interval. The agent was restarted
# near the top of this script, long enough ago for its first run to be in.
latest_value() { # <query> — the newest non-null point over the last 5 minutes
  local now; now=$(ozyd_now 2>/dev/null); [[ -n $now ]] || now=$(date +%s) # the VM clock, see ozyd_now
  curl -fsS --max-time 2 -G "$OZY_URL/api/v1/query" --data-urlencode "q=$1" \
    --data-urlencode "from=$((now - 300))" --data-urlencode "to=$((now + 60))" 2>/dev/null |
    python3 -c 'import json,sys
pts = [p for s in json.load(sys.stdin).get("series", []) for p in s["points"] if p[1] is not None]
print(max(pts)[1] if pts else "none")'
}
wait_rate() { # <query> — like wait_positive, but allowing for the two runs a rate needs
  local i
  for i in 1 2 3; do wait_positive "$1" 2>/dev/null && return 0; done
  wait_positive "$1"
}
wait_positive() { # <query>
  local v
  for _ in $(seq 1 30); do
    v=$(latest_value "$1")
    [[ $v != none ]] && python3 -c "import sys; sys.exit(0 if float('$v') > 0 else 1)" && return 0
    sleep 1
  done
  echo "$1: newest value '$v', want > 0" >&2
  return 1
}
host_q="{host:$(scripts/hostname.sh | tr '[:upper:]' '[:lower:]')}"
check "the host collector reports memory"      wait_positive "max:system.mem.total$host_q"
check "and disk space, per device"             wait_positive "max:system.disk.total$host_q by {device}"
# Only block devices: a folder shared in from the Mac is not a disk, and its
# path would be a tag value. Read from the newest bucket, so a series left by
# an older build does not count.
newest_devices() {
  local now; now=$(ozyd_now 2>/dev/null); [[ -n $now ]] || now=$(date +%s) # the VM clock, see ozyd_now
  curl -fsS --max-time 2 -G "$OZY_URL/api/v1/query" --data-urlencode "q=max:system.disk.total$host_q by {device}" \
    --data-urlencode "from=$((now - 300))" --data-urlencode "to=$((now + 60))" 2>/dev/null |
    python3 -c 'import json,sys
seen = {(s["tags"].get("device", "<none>"), p[0]) for s in json.load(sys.stdin).get("series", []) for p in s["points"] if p[1] is not None}
newest = max((t for _, t in seen), default=None)
print(" ".join(sorted(d for d, t in seen if t == newest)))'
}
only_block_devices() { local d; d=$(newest_devices); [[ -n $d ]] && ! grep -qv '^/dev/' <<<"${d// /$'\n'}" || { echo "devices: '$d'" >&2; return 1; }; }
check "and only block devices"                 only_block_devices
# A rate needs two runs, and is a gauge on the wire (ADR-0026): a store that
# typed the name otherwise refuses every point, which only this check sees.
check "and network rates"                      wait_rate "sum:system.net.bytes_rcvd$host_q"
check "the collector reports on itself"        wait_positive "sum:ozy.agent.collector.runs{collector:host}"
# Zero increase may arrive as 0 or not at all, depending on the reporter.
sum_window() { # <query> [seconds, default 300] — the sum of every point in the window (0 if none)
  local now; now=$(ozyd_now 2>/dev/null); [[ -n $now ]] || now=$(date +%s)
  curl -fsS --max-time 2 -G "$OZY_URL/api/v1/query" --data-urlencode "q=$1" \
    --data-urlencode "from=$((now - ${2:-300}))" --data-urlencode "to=$((now + 60))" 2>/dev/null |
    python3 -c 'import json,sys
print(sum(p[1] for s in json.load(sys.stdin).get("series", []) for p in s["points"] if p[1] is not None))'
}
# Errors are a count: one failing run is a single point, which the newest
# value would miss, so sum a window. 90s: several runs of this agent, but not
# the one before `make up` replaced it.
# No series at all also sums to 0, so the collector's runs must be there in
# the same window: zero errors from runs that happened, not from silence.
no_errors() { # <collector>
  local runs errs
  # A collector that just started (a discovered check) may have run before
  # its self-metrics were next reported: wait for the runs to arrive. Runs
  # and errors are reported together, so once runs are there, so are errors.
  local i
  for ((i = 0; i < 30; i++)); do
    runs=$(sum_window "sum:ozy.agent.collector.runs{collector:$1}" 90) || return 1
    python3 -c "import sys; sys.exit(0 if float('$runs') > 0 else 1)" && break
    sleep 1
  done
  python3 -c "import sys; sys.exit(0 if float('$runs') > 0 else 1)" || { echo "$1: no runs reported in 90s" >&2; return 1; }
  errs=$(sum_window "sum:ozy.agent.collector.errors{collector:$1}" 90) || return 1
  python3 -c "import sys; sys.exit(0 if float('$errs') == 0 else 1)" || { echo "$1: $errs errors in 90s" >&2; return 1; }
}
check "without errors"                         no_errors host
check "and no docker errors"                    no_errors docker

# The Docker collector and the event watcher (ADR-0028). A container that
# lives long enough to be polled shows up in container.*; one that exits at
# once is never polled, and is counted only because the watcher saw its die.
# The names are fixed, and deploy/agent.yaml rewrites them to drop
# container_id, so every smoke run adds to the same few series rather than
# minting new ones; the window starts when this section did (less one
# bucket, since points are stamped at their bucket's start), so a previous
# run's containers, minutes older, are not counted.
wait_since() { # <query> <seconds> — until the section's sum is positive
  local v i now
  for ((i = 0; i < $2; i++)); do
    now=$(ozyd_now 2>/dev/null) || now=$(date +%s)
    v=$(sum_window "$1" $((now - docker_since + 10)))
    [[ -n $v ]] && python3 -c "import sys; sys.exit(0 if float('$v') > 0 else 1)" && return 0
    sleep 1
  done
  echo "$1: sum '$v' since the section began, after $2s; want > 0" >&2
  return 1
}
smoke_ct=ozy-smoke
docker rm -f "$smoke_ct-long" "$smoke_ct-short" "$smoke_ct-redis" >/dev/null 2>&1 || true
trap 'docker rm -f "$smoke_ct-long" "$smoke_ct-short" "$smoke_ct-redis" >/dev/null 2>&1 || true' EXIT
docker_since=$(ozyd_now 2>/dev/null) || docker_since=$(date +%s)
# Started under check, so a failure (no busybox offline, no daemon) is a ✗
# with its reason, not set -e ending smoke without a summary.
check "a long-running test container starts"   docker run -d --name "$smoke_ct-long" busybox sh -c 'sleep 45; exit 3'
check "a test container that exits at once runs" sh -c "docker run --rm --name $smoke_ct-short busybox sh -c 'exit 7'; test \$? -eq 7"
check "a running container is polled"          wait_since "max:container.memory.usage{container_name:$smoke_ct-long}" 45
check "a container too brief to poll still counts" wait_since "sum:container.exits{container_name:$smoke_ct-short,exit_code:7}" 30
docker stop -t 0 "$smoke_ct-long" >/dev/null 2>&1 || true
check "and so does a stopped one"              wait_since "sum:container.exits{container_name:$smoke_ct-long}" 30
docker rm -f "$smoke_ct-long" >/dev/null 2>&1 || true

# Checks by autodiscovery: a Redis container that asks for the redis check
# with labels, on the agent's network so %%host%% is reachable. The check is
# named redis:<container>, and its metrics carry the container's tags.
docker run -d --rm --name "$smoke_ct-redis" --network ozymandias \
  --label 'ozy.check.redis.host=%%host%%' --label 'ozy.check.redis.port=%%port%%' \
  redis:7-alpine >/dev/null
check "a labelled container gets its check"    wait_since "max:redis.can_connect{container_name:$smoke_ct-redis}" 45
check "which reads the server"                 wait_since "max:redis.net.clients{container_name:$smoke_ct-redis}" 30
check "and runs without errors"                no_errors "redis:$smoke_ct-redis"
docker stop -t 1 "$smoke_ct-redis" >/dev/null 2>&1 || true

echo "smoke: $pass checks passed"
