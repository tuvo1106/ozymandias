// Package dockerapi is a small client for the Docker Engine API, spoken as
// plain HTTP over the daemon's unix socket. It is the part of the docker
// collector (docs/plan/M3-query-dashboards.md §3) that talks to the daemon;
// it knows nothing about metrics, tags or schedules, so the collector can be
// tested against canned values and this package against a fake daemon.
//
// # Why raw HTTP
//
// The Docker CLI is itself just an HTTP client: `docker ps` is
// GET /containers/json on /var/run/docker.sock. The official Go client
// library pulls in a large dependency tree to do the same thing, and writing
// the four calls we need is the point of this project (AGENTS.md §4). The
// transport is an ordinary net/http client whose dialer ignores the host in
// the URL and connects to the socket instead; every request is sent to
// "http://docker/...", and the host part is never resolved.
//
// Paths are unversioned ("/containers/json", not "/v1.41/containers/json").
// An unversioned path gets the daemon's own current API version. Pinning an
// old version looks safer but is not: daemons drop old versions (Engine 29
// refuses anything below 1.44), and the fields read here have kept their
// shape across every version since stats gained online_cpus.
//
// # Four calls
//
//   - [Client.ListContainers]: GET /containers/json. The running
//     containers, their names, images and labels: discovery.
//   - [Client.Stats]: GET /containers/{id}/stats?stream=false&one-shot=true.
//     One snapshot of a container's cgroup counters, answered at once.
//   - [Client.Inspect]: GET /containers/{id}/json. State that stats
//     does not carry: exit code, OOM kill, start and finish times.
//   - [Client.Events]: GET /events. A long-lived stream of lifecycle events
//     (start, die), the only way to see a container that lives for less
//     than a polling interval.
//
// # One-shot stats, and where CPU % comes from
//
// A container's CPU counter is cumulative nanoseconds of CPU time used
// since it started (cpu_stats.cpu_usage.total_usage). A percentage needs two
// samples: how much the container used between them, over how much CPU time
// the whole machine had in the same window. With stream=false alone the
// daemon provides both halves in one response — it samples the cgroup
// twice, a second apart, and returns the earlier sample as precpu_stats —
// but holds every request for that second: at eight in flight, a host with
// a hundred containers takes over ten seconds to read. With one-shot=true
// (API 1.41) it answers at once with one sample, and the caller keeps the
// previous one ([CPUPercentBetween]); the collector polls every 15s anyway,
// so its window is simply the interval (ADR-0030). This package uses
// one-shot only and does not decode precpu_stats. With prev the previous
// answer and cur this one:
//
//	cpu_delta    = cur.cpu_stats.cpu_usage.total_usage - prev.cpu_stats.cpu_usage.total_usage
//	system_delta = cur.cpu_stats.system_cpu_usage    - prev.cpu_stats.system_cpu_usage
//	cpu %        = cpu_delta / system_delta × online_cpus × 100
//
// system_cpu_usage is the host's total CPU time summed over all cores, so
// cpu_delta/system_delta is "this container's share of the whole machine".
// Multiplying by online_cpus turns that into "% of one core", the unit
// `docker stats` uses: a container spinning two cores on an eight-core host
// reads 200%, not 25%. [CPUPercentBetween] does this and says
// when they cannot: with no earlier sample, or a zero or backwards delta
// (the container restarted), there is no meaningful ratio.
//
// # Memory: cgroup v1 and v2
//
// memory_stats.usage is the cgroup's charged memory, and it includes page
// cache: a container that read a large file looks like it is using that
// file's size in memory, though the kernel can drop that cache at any time.
// `docker stats` therefore subtracts inactive file cache, and so does
// [MemoryStats.Breakdown]. The name of that counter and of its neighbours
// depends on the cgroup version the host runs:
//
//	            cgroup v1                 cgroup v2
//	inactive    total_inactive_file       inactive_file
//	rss         rss (total_rss)           anon
//	cache       cache                     file
//
// Docker passes the kernel's own keys through, so the version is recognised
// by which keys are present. Modern Linux (and Colima's VM) is v2.
//
// # Events
//
// GET /events answers with a response that never ends: one JSON object per
// line, written as things happen, until the client goes away or the daemon
// restarts. [Client.Events] filters to container start, oom and die events on the
// daemon side and hands each to a callback. When the stream ends for any
// reason the caller reconnects with since set to the last event's time
// ([Event.At]). since is inclusive, so the event at that instant is
// delivered again, and the caller must tolerate it.
//
// Socket access is root-equivalent on the host (anyone who can talk to the
// daemon can start a privileged container), so this package only ever reads.
package dockerapi
