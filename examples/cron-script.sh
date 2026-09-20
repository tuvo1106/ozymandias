#!/bin/sh
# Send a metric to ozymandias from a shell script — no SDK, no library, just a
# UDP datagram. This is the whole point of a text protocol: anything that can
# open a socket is an instrumented app.
#
# A cron job that reports how long it took and whether it worked:
#
#   */5 * * * * /path/to/cron-script.sh
#
# Usage:
#   OZY_AGENT_HOST=agent ./cron-script.sh
#
#   OZY_AGENT_HOST  agent hostname (default: localhost). Unset is not an
#                        error — see "inert" below.
#   OZY_AGENT_PORT  statsd port (default: 8125)
#   JOB                  job name for the `job:` tag (default: example)
#
# Inert by default: with OZY_AGENT_HOST unset this still runs the work and
# still exits with the work's status. Monitoring that can break the thing it
# monitors is worse than no monitoring, so every send is best-effort and every
# failure to send is ignored (`|| true`). UDP gives us that for free — there is
# no connection to fail and no reply to wait for.
#
# POSIX sh, not bash: this is meant to be copied into a busybox container.
set -u

HOST=${OZY_AGENT_HOST:-localhost}
PORT=${OZY_AGENT_PORT:-8125}
JOB=${JOB:-example}

# send <metric-line>...  — one datagram, newline-separated, never fatal.
#
# -w1 caps how long nc lingers waiting for a reply that a UDP listener will
# never send. Not -w0: BSD nc reads that as "don't wait", but busybox nc reads
# it as "wait forever", which would hang the cron job on the one thing that was
# supposed to be fire-and-forget.
send() {
  printf '%s\n' "$@" | nc -u -w1 "$HOST" "$PORT" 2>/dev/null || true
}

# --- the actual work -----------------------------------------------------------
start=$(date +%s)
# Replace this with the job. Its exit status decides the `status:` tag below.
sleep 1
status=$?
elapsed=$(( $(date +%s) - start ))

# --- report --------------------------------------------------------------------
# A counter per outcome (so a missing success is visible as an absence of
# increments, not merely as a gauge that stopped moving), and a timer for the
# duration. `|ms` is a histogram: the agent turns it into avg/min/max/p95
# without the script knowing anything about percentiles.
if [ "$status" -eq 0 ]; then
  outcome=ok
else
  outcome=error
fi
send "cron.job.runs:1|c|#job:$JOB,status:$outcome" \
     "cron.job.duration:$((elapsed * 1000))|ms|#job:$JOB"

exit "$status"
