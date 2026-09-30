# ADR-0028: The agent reads the Docker socket, through its group

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

M3 §3 has the agent report every container on its host: resource use,
exits, lifetimes. The Docker daemon is the only thing that knows which
containers exist, what they are called and when they started, and it
answers over its API on a unix socket (`/var/run/docker.sock`). The cgroup
files under `/sys/fs/cgroup` carry the same counters without the daemon,
but not the names, images or compose labels that make the numbers readable,
and not the event stream that sees a container too short-lived for any poll.

The agent image runs as a non-root user. The socket is owned by
`root:<docker group>`, mode 660, and the docker group's id is whatever the
Docker host picked: 991 under Colima, something else under Docker Desktop
or a Linux host. A container sees the VM's socket, not the Mac's, so the Mac
cannot tell us the number.

Access to the socket is root on the Docker host: anything that can call the
API can start a privileged container with the host's filesystem mounted.
Mounting it `:ro` does not change that; it stops the socket file being
replaced, not API calls through it.

## Decision

The compose stack mounts the socket into the agent (`:ro`, for what it is
worth) and gives the agent the socket's group with `group_add`, rather than
running it as root. `make up` finds the group id by asking a throwaway
container (`docker run --rm -v /var/run/docker.sock:/s busybox stat -c %g
/s`) and passes it as `OZY_DOCKER_GID`; without make the default is 0, root's
group, which owns the socket on hosts that have no docker group. The agent
uses only GET endpoints (list, stats, inspect, events), and the client has
no code that could call anything else.

`collectors.docker.enabled: false` turns it off, and an agent that cannot
reach the socket logs it once and keeps running: container metrics are
absent, not broken.

## Alternatives considered

| Option | Why not |
|---|---|
| Run the agent as root | Grants everything root has in the container (every file, every capability the runtime left) to get one socket; the group grants the socket and nothing more |
| A socket proxy (e.g. tecnativa/docker-socket-proxy) allowing only GETs | The right answer where the host matters; here it is another container and image in a local learning stack to remove a risk the agent's own code does not take. Noted in docs/operations.md for anyone deploying it for real (the client speaks unix sockets only, so the proxy must serve one) |
| Read cgroup files instead of the API | No names, images, labels or events; a container becomes a hash |
| Hard-code gid 991 | Correct only under Colima; Docker Desktop and Linux hosts differ, and a wrong gid fails silently (permission denied, logged once) |
| `docker context`'s socket on the Mac | The daemon is in the VM; the container needs the VM's socket, which compose mounts by the VM's path |

## Consequences

- The agent container holds root-equivalent access to the Docker VM. That is
  acceptable for a local stack whose Docker VM exists to run it; a real
  deployment should put a GET-only proxy in front.
- `make up` pulls `busybox` once, for the `stat`.
- `docker compose up` run by hand, without make, uses gid 0 and gets
  permission denied on Colima; the agent logs that once and the rest of it
  works. `make up` is the documented way to start the stack.
- `make dev` runs the agent natively as the user, who already has the socket
  through the docker context; `scripts/dev.sh` points the agent at that
  socket.
