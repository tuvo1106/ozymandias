// Package agent assembles the ozymandias agent: the per-host process that
// collects telemetry near where it is produced and forwards it to ozyd.
//
// Why an agent at all, instead of apps sending straight to the server? Three
// reasons the later milestones make concrete:
//
//   - Aggregation at the edge (M1): an app emits one statsd packet per event;
//     the agent turns thousands of them into one point per series per 10
//     seconds, so the network and the server see a fraction of the volume.
//   - Things only a local process can see (M3/M4): host CPU and memory, other
//     containers' stats and stdout, log files on disk.
//   - Isolation (M7): the app hands data to a local process over UDP or
//     loopback and moves on. Retries, buffering and a slow or dead server are
//     the agent's problem, never the app's.
//
// The subpackages are the agent's pipeline stages; this package wires them
// together and owns the agent's own HTTP listener. In M0 it serves only
// GET /healthz and GET /debug/vars.
package agent
