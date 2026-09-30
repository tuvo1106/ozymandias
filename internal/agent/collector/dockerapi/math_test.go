package dockerapi

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func loadStats(t *testing.T, name string) Stats {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var s Stats
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCPUPercent_FromCannedPayloads(t *testing.T) {
	// v2: 0.5s of CPU over 4s of machine time on 4 cores = half a core.
	// v1: no online_cpus (an old daemon), so the per-CPU list counts the
	// cores: 0.1s over 2s on 2 cores = a tenth of a core.
	for _, tc := range []struct {
		file string
		want float64
	}{
		{"stats-cgroupv2.json", 50},
		{"stats-cgroupv1.json", 10},
	} {
		got, ok := CPUPercent(loadStats(t, tc.file))
		if !ok || !near(got, tc.want) {
			t.Errorf("%s: CPUPercent = %v, %v; want %v", tc.file, got, ok, tc.want)
		}
	}
}

func TestCPUPercentBetween_TwoSeparateAnswers(t *testing.T) {
	prev := loadStats(t, "stats-cgroupv2.json")
	cur := prev
	cur.CPUStats.CPUUsage.TotalUsage += 8_000_000_000 // 8s of CPU…
	cur.CPUStats.SystemUsage += 10_000_000_000        // …in 10s of machine time
	got, ok := CPUPercentBetween(prev, cur)
	if !ok || !near(got, 320) { // 0.8 of the machine × 4 cores = 3.2 cores
		t.Fatalf("CPUPercentBetween = %v, %v; want 320", got, ok)
	}
}

func TestCPUPercent_NoAnswerIsNotZero(t *testing.T) {
	base := loadStats(t, "stats-cgroupv2.json")
	cases := map[string]func(*Stats){
		"first sample: precpu empty": func(s *Stats) { s.PreCPUStats = CPUStats{} },
		"same instant":               func(s *Stats) { s.CPUStats.SystemUsage = s.PreCPUStats.SystemUsage },
		"system counter backwards":   func(s *Stats) { s.CPUStats.SystemUsage = s.PreCPUStats.SystemUsage - 1 },
		"container counter reset":    func(s *Stats) { s.CPUStats.CPUUsage.TotalUsage = 1 },
		"no core count at all":       func(s *Stats) { s.CPUStats.OnlineCPUs = 0; s.CPUStats.CPUUsage.PercpuUsage = nil },
	}
	for name, mutate := range cases {
		s := base
		mutate(&s)
		if got, ok := CPUPercent(s); ok {
			t.Errorf("%s: CPUPercent = %v, want no answer", name, got)
		}
	}
	// And an idle container is an answer: 0%, ok.
	s := base
	s.CPUStats.CPUUsage.TotalUsage = s.PreCPUStats.CPUUsage.TotalUsage
	if got, ok := CPUPercent(s); !ok || got != 0 {
		t.Errorf("idle: CPUPercent = %v, %v; want 0, true", got, ok)
	}
}

func TestMemoryBreakdown(t *testing.T) {
	for _, tc := range []struct {
		file string
		want Memory
	}{
		{"stats-cgroupv2.json", Memory{Usage: 157286400 - 31457280, Limit: 536870912, RSS: 94371840, Cache: 52428800, CgroupV2: true}},
		{"stats-cgroupv1.json", Memory{Usage: 83886080 - 16777216, Limit: 2147483648, RSS: 41943040, Cache: 33554432}},
	} {
		got, ok := loadStats(t, tc.file).MemoryStats.Breakdown()
		if !ok || got != tc.want {
			t.Errorf("%s: Breakdown = %+v, %v; want %+v", tc.file, got, ok, tc.want)
		}
	}
}

func TestMemoryBreakdown_EdgeCases(t *testing.T) {
	if m, ok := loadStats(t, "stats-stopped.json").MemoryStats.Breakdown(); ok {
		t.Errorf("stopped container: %+v, want no answer rather than zero bytes", m)
	}
	// Inactive cache sampled a moment after usage can exceed it; unsigned
	// subtraction would wrap to ~1.8e19 bytes.
	m, _ := MemoryStats{Usage: 100, Stats: map[string]uint64{"anon": 50, "inactive_file": 200}}.Breakdown()
	if m.Usage != 100 {
		t.Errorf("inactive > usage: Usage = %d, want 100 unchanged", m.Usage)
	}
	// v1 fallbacks: total_rss when rss is absent, inactive_file when
	// total_inactive_file is.
	m, _ = MemoryStats{Usage: 100, Stats: map[string]uint64{"total_rss": 40, "inactive_file": 30}}.Breakdown()
	if m.RSS != 40 || m.Usage != 70 || m.CgroupV2 {
		t.Errorf("v1 fallbacks: %+v", m)
	}
}

func TestBlockIOAndNetwork(t *testing.T) {
	for _, tc := range []struct {
		file                 string
		read, write, rx, tx  uint64
		pids, throttledCount uint64
	}{
		{"stats-cgroupv2.json", 10485760 + 1048576, 4194304, 1234567 + 1000, 7654321 + 2000, 23, 6},
		// v1 also reports Sync/Async/Total; counting them would double.
		{"stats-cgroupv1.json", 2048, 4096, 500, 700, 5, 0},
	} {
		s := loadStats(t, tc.file)
		r, w, ioOK := s.BlockIO()
		rx, tx, netOK := s.NetworkBytes()
		if r != tc.read || w != tc.write || rx != tc.rx || tx != tc.tx || !ioOK || !netOK {
			t.Errorf("%s: io %d/%d net %d/%d, want %d/%d %d/%d", tc.file, r, w, rx, tx, tc.read, tc.write, tc.rx, tc.tx)
		}
		if s.PidsStats.Current != tc.pids || s.CPUStats.ThrottlingData.ThrottledPeriods != tc.throttledCount {
			t.Errorf("%s: pids %d throttled %d", tc.file, s.PidsStats.Current, s.CPUStats.ThrottlingData.ThrottledPeriods)
		}
	}
}

func TestSampled(t *testing.T) {
	if !loadStats(t, "stats-cgroupv2.json").Sampled() {
		t.Error("a real sample reads as not sampled")
	}
	if loadStats(t, "stats-stopped.json").Sampled() {
		t.Error("a stopped container's all-zero answer reads as sampled")
	}
}

func TestParseImage(t *testing.T) {
	const digest = "sha256:4f1c9e2b7a3d6c8e0f5b1a9d2c7e4b8f6a0d3c5e9b2f7a1c4d8e6b0f3a5c9e2d"
	for _, tc := range []struct{ ref, name, tag string }{
		{"redis", "redis", "latest"},
		{"redis:7.2-alpine", "redis", "7.2-alpine"},
		{"shop/api:1.4.2", "shop/api", "1.4.2"},
		{"localhost:5000/app", "localhost:5000/app", "latest"},
		{"localhost:5000/app:1.0", "localhost:5000/app", "1.0"},
		{"ghcr.io/org/team/app:v2", "ghcr.io/org/team/app", "v2"},
		{"redis@" + digest, "redis", ""},
		{"redis:7@" + digest, "redis", "7"},
		{"localhost:5000/app@" + digest, "localhost:5000/app", ""},
		{digest, "4f1c9e2b7a3d", ""},
	} {
		name, tag := ParseImage(tc.ref)
		if name != tc.name || tag != tc.tag {
			t.Errorf("ParseImage(%q) = %q, %q; want %q, %q", tc.ref, name, tag, tc.name, tc.tag)
		}
	}
}

func TestShortID(t *testing.T) {
	for in, want := range map[string]string{
		"8dfafdbc3a40c7b5f8e2a9c4d1e6b0a3": "8dfafdbc3a40",
		"sha256:9a2d7b4f1e8c5a3d0b6f":      "9a2d7b4f1e8c",
		"abc":                              "abc",
		"":                                 "",
	} {
		if got := ShortID(in); got != want {
			t.Errorf("ShortID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerStateLifetime(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 17, 10, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		s    ContainerState
		want time.Duration
		ok   bool
	}{
		{"exited", ContainerState{StartedAt: t0, FinishedAt: t0.Add(2500 * time.Millisecond)}, 2500 * time.Millisecond, true},
		{"running", ContainerState{Running: true, StartedAt: t0}, 0, false},
		{"never started", ContainerState{FinishedAt: t0}, 0, false},
		{"finish before start", ContainerState{StartedAt: t0, FinishedAt: t0.Add(-time.Second)}, 0, false},
	} {
		got, ok := tc.s.Lifetime()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: Lifetime = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
