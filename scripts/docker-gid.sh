#!/usr/bin/env bash
# Prints the Docker socket's group as the Docker VM sees it, for the agent's
# group_add (ADR-0028). It is read from a throwaway container because the
# VM's /var/run is not the Mac's.
#
# The container is named, because the agent reports every container's exit
# by name, and an unnamed one gets a new random name (and so a new series)
# every time. A second `make up` in another worktree can hold that name for
# a moment, so a name clash is retried. Any other failure (no daemon, no
# busybox image while offline) prints nothing and says why on stderr:
# compose then falls back to group 0, which is right on a host with no
# docker group and wrong under Colima (991), where the agent would be
# refused the socket.
set -u
err=$(mktemp)
trap 'rm -f "$err"' EXIT
for _ in 1 2 3 4 5; do
  if gid=$(docker run --rm --name ozymandias-gid-probe -v /var/run/docker.sock:/s busybox stat -c %g /s 2>"$err") && [[ -n $gid ]]; then
    echo "$gid"
    exit 0
  fi
  grep -q 'already in use' "$err" || break
  sleep 1
done
echo "warning: could not read the Docker socket's group; the agent gets group 0 and may be refused the socket (ADR-0028): $(tail -1 "$err")" >&2
