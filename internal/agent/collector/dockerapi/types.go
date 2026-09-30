package dockerapi

import (
	"strings"
	"time"
)

// Container is one entry of GET /containers/json: only the fields the
// collector reads. Everything else the daemon sends is ignored by the
// decoder, which is what keeps this working across API versions.
type Container struct {
	ID string `json:"Id"`
	// Names are the container's names, each with a leading "/" (a legacy
	// of links). Use [Container.Name].
	Names []string `json:"Names"`
	// Image is the reference the container was started from, as the user
	// wrote it ("redis:7"), or the image id if that reference has since
	// been re-pointed or removed. See [ParseImage].
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// Name returns the container's first name without the leading "/", or its
// short id if it somehow has none.
func (c Container) Name() string {
	for _, n := range c.Names {
		if n = strings.TrimPrefix(n, "/"); n != "" {
			return n
		}
	}
	return ShortID(c.ID)
}

// ContainerJSON is the part of GET /containers/{id}/json the agent uses.
type ContainerJSON struct {
	State ContainerState `json:"State"`
}

// ContainerState is the part of a container's lifecycle state the agent
// reads: when it started (for uptime, and for a lifetime whose start event
// was missed) and whether the kernel's OOM killer ended it. The daemon
// writes "0001-01-01T00:00:00Z" for a start that has not happened, which
// decodes as the zero time.Time.
type ContainerState struct {
	OOMKilled bool      `json:"OOMKilled"`
	StartedAt time.Time `json:"StartedAt"`
}

// Stats is one GET /containers/{id}/stats?stream=false answer. The counters
// are cumulative since the container started; see the package doc for how
// CPUStats and PreCPUStats combine into a percentage.
type Stats struct {
	// Read is when the daemon sampled. It is the zero time when the
	// container was not running by the time the daemon looked, and then
	// every counter is zero too: see [Stats.Sampled].
	Read        time.Time               `json:"read"`
	CPUStats    CPUStats                `json:"cpu_stats"`
	MemoryStats MemoryStats             `json:"memory_stats"`
	Networks    map[string]NetworkStats `json:"networks"`
	BlkioStats  BlkioStats              `json:"blkio_stats"`
	PidsStats   PidsStats               `json:"pids_stats"`
}

// Sampled reports whether the daemon actually read the container's
// cgroups. A container that stopped between the list and the stats call
// gets a 200 with an all-zero body; charting those zeros would say "used
// nothing", which is not what happened.
func (s Stats) Sampled() bool { return !s.Read.IsZero() }

// CPUStats is one CPU sample.
type CPUStats struct {
	CPUUsage CPUUsage `json:"cpu_usage"`
	// SystemUsage is the host's total CPU time, summed over every core, in
	// nanoseconds. The denominator of the percentage.
	SystemUsage uint64 `json:"system_cpu_usage"`
	// OnlineCPUs is the number of cores the host has online. Old daemons
	// omit it; [CPUPercent] then counts PercpuUsage instead.
	OnlineCPUs     uint32         `json:"online_cpus"`
	ThrottlingData ThrottlingData `json:"throttling_data"`
}

// CPUUsage is the container's CPU time used, in nanoseconds.
type CPUUsage struct {
	TotalUsage uint64 `json:"total_usage"`
	// PercpuUsage is per-core usage. cgroup v2 does not report it.
	PercpuUsage []uint64 `json:"percpu_usage"`
}

// ThrottlingData says how often a container with a CPU limit hit it: of
// Periods scheduling periods, ThrottledPeriods ended with the container
// out of quota, for ThrottledTime nanoseconds in total. All cumulative.
type ThrottlingData struct {
	Periods          uint64 `json:"periods"`
	ThrottledPeriods uint64 `json:"throttled_periods"`
	ThrottledTime    uint64 `json:"throttled_time"`
}

// MemoryStats is the container's memory cgroup: Usage and Limit in bytes,
// and the kernel's breakdown in Stats, whose keys depend on the cgroup
// version (see the package doc and [MemoryStats.Breakdown]).
type MemoryStats struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

// NetworkStats is one interface's cumulative traffic.
type NetworkStats struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// BlkioStats is the container's block I/O.
type BlkioStats struct {
	IoServiceBytesRecursive []BlkioEntry `json:"io_service_bytes_recursive"`
}

// BlkioEntry is cumulative bytes for one device and one operation. cgroup
// v1 writes Op as "Read"/"Write" (plus "Sync", "Async", "Total"); v2 as
// "read"/"write".
type BlkioEntry struct {
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

// PidsStats is the number of processes (threads, on Linux) in the
// container.
type PidsStats struct {
	Current uint64 `json:"current"`
	Limit   uint64 `json:"limit"`
}

// Event is one line of GET /events.
type Event struct {
	Type   string `json:"Type"`   // "container"
	Action string `json:"Action"` // "start", "die", …
	Actor  Actor  `json:"Actor"`
	// Time and TimeNano are when it happened, in unix seconds and unix
	// nanoseconds. Use [Event.At].
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`

	// Legacy top-level fields; [DecodeEvent] folds them into Action and
	// Actor.ID.
	Status string `json:"status"`
	ID     string `json:"id"`
}

// Actor is what an event happened to. For a container, Attributes carries
// its labels plus "name", "image" and, on die, "exitCode".
type Actor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// At is when the event happened, at the best precision the daemon gave.
// It is what to pass back as since to resume the stream.
func (e Event) At() time.Time {
	if e.TimeNano != 0 {
		return time.Unix(0, e.TimeNano)
	}
	return time.Unix(e.Time, 0)
}
