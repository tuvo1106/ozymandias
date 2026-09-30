#!/usr/bin/env bash
# Prints the name ozymandias tags this machine's data with: OZY_HOSTNAME if
# set, else OZY_AGENT_HOSTNAME (older docs said to set that one), else a name
# that does not change with the network. Empty counts as unset. This is the
# one place the launchers (Makefile, dev.sh, smoke.sh) get the name from, so
# they cannot disagree about it.
#
# Not `hostname -s`: on macOS that comes from DHCP/reverse DNS when the
# network offers one, so one laptop has been "Tus-MacBook-Pro" at one moment
# and "Mac" (from Mac.attlocal.net) the next, and each change starts a new
# host value and a new copy of every series. LocalHostName is the machine's
# own setting (System Settings → Sharing) and stays put. Elsewhere, the
# short hostname is already the machine's own.
set -euo pipefail
if [[ -n ${OZY_HOSTNAME:-} ]]; then
  echo "$OZY_HOSTNAME"
elif [[ -n ${OZY_AGENT_HOSTNAME:-} ]]; then
  echo "$OZY_AGENT_HOSTNAME"
elif [[ $(uname) == Darwin ]] && name=$(scutil --get LocalHostName 2>/dev/null) && [[ -n $name ]]; then
  echo "$name"
else
  hostname -s
fi
