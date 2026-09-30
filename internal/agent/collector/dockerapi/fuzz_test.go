package dockerapi

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The daemon is trusted, but not to be well-formed: a version we have not
// seen, a proxy in between, or a truncated read must give an error or a
// value, never a panic, and the maths on whatever decodes must stay finite.

func seed(f *testing.F, files ...string) {
	for _, name := range files {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
}

func FuzzDecodeStats(f *testing.F) {
	seed(f, "stats-cgroupv2.json", "stats-cgroupv1.json", "stats-stopped.json")
	f.Add([]byte(`{"cpu_stats":{"cpu_usage":{"total_usage":18446744073709551615},"system_cpu_usage":1,"online_cpus":4294967295},"precpu_stats":{"system_cpu_usage":0}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s Stats
		if json.Unmarshal(data, &s) != nil {
			return
		}
		var pre struct {
			CPU CPUStats `json:"precpu_stats"`
		}
		_ = json.Unmarshal(data, &pre)
		if pct, ok := CPUPercentBetween(Stats{CPUStats: pre.CPU}, s); ok && (math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0) {
			t.Fatalf("CPUPercentBetween = %v from %s", pct, data)
		}
		if m, ok := s.MemoryStats.Breakdown(); ok && m.Usage > s.MemoryStats.Usage {
			t.Fatalf("Breakdown usage %d exceeds charged usage %d: subtraction wrapped", m.Usage, s.MemoryStats.Usage)
		}
		s.BlockIO()
		s.NetworkBytes()
	})
}

func FuzzDecodeContainers(f *testing.F) {
	seed(f, "containers.json")
	f.Add([]byte(`[{"Names":["/"],"Id":""}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var cs []Container
		if json.Unmarshal(data, &cs) != nil {
			return
		}
		for _, c := range cs {
			c.Name()
			name, _ := ParseImage(c.Image)
			if c.Image != "" && name == "" && c.Image[0] != '@' && c.Image[0] != ':' {
				t.Fatalf("ParseImage(%q) lost the name", c.Image)
			}
		}
	})
}

func FuzzDecodeEvent(f *testing.F) {
	b, err := os.ReadFile("testdata/events.jsonl")
	if err != nil {
		f.Fatal(err)
	}
	for _, line := range splitLines(b) {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		ev, err := DecodeEvent(line)
		if err != nil {
			return
		}
		ev.ExitCode()
		ev.At()
		// The fold is one-way and total: legacy fields never override the
		// new ones, and fill them when they are empty.
		if ev.Action == "" && ev.Status != "" || ev.Actor.ID == "" && ev.ID != "" {
			t.Fatalf("legacy fields not folded: %+v", ev)
		}
	})
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
