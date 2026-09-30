// Package host is the collector for the machine the agent runs on: CPU,
// load, memory, swap, disk space, disk I/O, network traffic and uptime,
// reported as system.* (docs/metrics-catalog.md).
//
// # Where the numbers come from
//
// The kernel keeps running totals — CPU seconds per state since boot, bytes
// per interface since it came up, reads and writes per disk — and gopsutil
// reads them (/proc on Linux, sysctl and IOKit on macOS). Totals are turned
// into what a dashboard wants by differencing two readings:
//
//   - CPU: each state's share of the time that passed, as a percentage of
//     all cores together. The first run has one reading and emits none.
//   - disk I/O and network: per-second rates, via collector.Rates, which
//     skips a reading across a counter reset instead of reporting a
//     negative spike.
//
// Memory, swap, disk space, load and uptime are already levels and are
// reported as gauges.
//
// # Which machine
//
// Inside a container these describe whatever kernel the container shares:
// under Colima or Docker Desktop, the Linux VM, not the Mac. Running the
// agent natively (`make dev`) reports the Mac itself. Disk space and
// network in a container are the container's own view (its mounts and its
// network namespace) unless it is given the host's.
//
// # Choices worth knowing
//
//   - system.mem.usable is "available" (free plus reclaimable cache), the
//     number that says whether the machine is short of memory; free alone is
//     near zero on any healthy Linux host.
//   - A device mounted in several places is reported once, so summing
//     system.disk.* over devices does not count a disk twice.
//   - Interfaces that never moved a packet, and macOS's virtual ones
//     (collectors.host.exclude_interfaces), are skipped: each would be eight
//     series of zeros.
package host
