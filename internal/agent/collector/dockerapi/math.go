package dockerapi

import "strings"

// CPUPercent is the container's CPU use over the daemon's own sampling
// window (precpu_stats → cpu_stats), in % of one core. ok is false when
// there is no answer to give, rather than a 0 that would chart as "idle":
//
//   - on the first sample of a container, precpu_stats is empty;
//   - when the system delta is zero, the two samples are the same instant;
//   - when either counter went backwards (the container restarted between
//     samples, so its counter reset), the difference is not usage.
func CPUPercent(s Stats) (pct float64, ok bool) {
	return cpuPercent(s.PreCPUStats, s.CPUStats)
}

// CPUPercentBetween is [CPUPercent] across two separate stats answers,
// prev then cur, for a caller that keeps its own history (for example with
// the daemon's one-shot mode, which skips the second sample).
func CPUPercentBetween(prev, cur Stats) (pct float64, ok bool) {
	return cpuPercent(prev.CPUStats, cur.CPUStats)
}

func cpuPercent(prev, cur CPUStats) (float64, bool) {
	if prev.SystemUsage == 0 || cur.SystemUsage <= prev.SystemUsage ||
		cur.CPUUsage.TotalUsage < prev.CPUUsage.TotalUsage {
		return 0, false
	}
	cpus := float64(cur.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(cur.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		return 0, false
	}
	cpuDelta := float64(cur.CPUUsage.TotalUsage - prev.CPUUsage.TotalUsage)
	sysDelta := float64(cur.SystemUsage - prev.SystemUsage)
	return cpuDelta / sysDelta * cpus * 100, true
}

// Memory is a container's memory in bytes, normalised across cgroup
// versions.
type Memory struct {
	// Usage is charged memory minus inactive file cache: what `docker stats`
	// shows, and the number that approaches Limit before an OOM kill.
	Usage uint64
	// Limit is the container's limit, or the host's memory when it has none.
	Limit uint64
	// RSS is anonymous memory (heap, stacks): v1 "rss", v2 "anon".
	RSS uint64
	// Cache is page cache: v1 "cache", v2 "file".
	Cache uint64
	// CgroupV2 says which key set the numbers came from.
	CgroupV2 bool
}

// Breakdown normalises the memory stats of either cgroup version (package
// doc). ok is false for an empty answer (a container that was not running
// when sampled), so it is not reported as zero bytes.
func (m MemoryStats) Breakdown() (mem Memory, ok bool) {
	if m.Usage == 0 && len(m.Stats) == 0 {
		return Memory{}, false
	}
	mem = Memory{Usage: m.Usage, Limit: m.Limit}
	var inactive uint64
	if _, v2 := m.Stats["anon"]; v2 {
		mem.CgroupV2 = true
		mem.RSS, mem.Cache, inactive = m.Stats["anon"], m.Stats["file"], m.Stats["inactive_file"]
	} else {
		mem.RSS = m.Stats["rss"]
		if mem.RSS == 0 {
			mem.RSS = m.Stats["total_rss"]
		}
		mem.Cache = m.Stats["cache"]
		inactive = m.Stats["total_inactive_file"]
		if inactive == 0 {
			inactive = m.Stats["inactive_file"]
		}
	}
	// Subtract only when it makes sense: a counter sampled a moment after
	// usage can exceed it, and unsigned subtraction would wrap.
	if inactive < mem.Usage {
		mem.Usage -= inactive
	}
	return mem, true
}

// BlockIO sums the container's cumulative bytes read and written over all
// devices. Only the read and write operations count; v1 also reports
// "Total", "Sync" and "Async", which would count the same bytes twice.
func (s Stats) BlockIO() (read, write uint64) {
	for _, e := range s.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			read += e.Value
		case "write":
			write += e.Value
		}
	}
	return read, write
}

// NetworkBytes sums cumulative bytes received and sent over every
// interface of the container. A container on host networking has none.
func (s Stats) NetworkBytes() (rx, tx uint64) {
	for _, n := range s.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	return rx, tx
}

// ShortID is the first 12 characters of an id, the form `docker ps` shows.
// Short enough for a tag, long enough to be unique on one host.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ParseImage splits an image reference into the name and tag the collector
// tags series with:
//
//	redis                        → redis, latest
//	redis:7.2                    → redis, 7.2
//	localhost:5000/app:1.0       → localhost:5000/app, 1.0
//	localhost:5000/app           → localhost:5000/app, latest
//	redis@sha256:<digest>        → redis, ""
//	redis:7@sha256:<digest>      → redis, 7
//	sha256:<id>                  → <short id>, ""
//
// The tag is the text after the last ":" that follows the last "/", so a
// registry's port is never mistaken for one. A reference pinned only by
// digest has no tag, and gets "" rather than "latest", which it may not be;
// likewise a bare image id, which is what the daemon reports when the name
// the container started from has since been re-pointed or removed. Callers
// leave an empty tag out.
func ParseImage(ref string) (name, tag string) {
	if strings.HasPrefix(ref, "sha256:") {
		return ShortID(ref), ""
	}
	digest := false
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, digest = ref[:i], true
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	if digest {
		return ref, ""
	}
	return ref, "latest"
}
