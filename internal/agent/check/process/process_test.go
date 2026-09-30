package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var t0 = time.Unix(1_790_000_000, 0)

// table is a scripted process table.
type table struct {
	procs   []Proc
	usage   map[int32]Usage
	listErr error
}

func (t *table) List(context.Context) ([]Proc, error) { return t.procs, t.listErr }
func (t *table) Usage(_ context.Context, pid int32) (Usage, error) {
	u, ok := t.usage[pid]
	if !ok {
		return Usage{}, errors.New("no such process")
	}
	return u, nil
}

func machine() *table {
	return &table{
		procs: []Proc{
			{PID: 10, Name: "nginx", Cmdline: "nginx: master process /usr/sbin/nginx"},
			{PID: 11, Name: "nginx", Cmdline: "nginx: worker process"},
			{PID: 12, Name: "nginx", Cmdline: "nginx: worker process"}, // exits before its usage is read
			{PID: 20, Name: "python3", Cmdline: "python3 -m uvicorn app:api --port 8000"},
			{PID: 30, Name: "postgres", Cmdline: "postgres -D /data"},
		},
		usage: map[int32]Usage{
			10: {CPUSeconds: 1, RSS: 1000, Threads: 1, FDs: 10, FDsOK: true, Started: 1},
			11: {CPUSeconds: 5, RSS: 3000, Threads: 2, FDs: 20, FDsOK: true, Started: 2},
			20: {CPUSeconds: 7, RSS: 500, Threads: 4, FDs: 9, FDsOK: true, Started: 3},
			30: {CPUSeconds: 2, RSS: 800, Threads: 1, FDsOK: false, Started: 4},
		},
	}
}

type got map[string]collector.Metric

func run(t *testing.T, c collector.Collector) (got, error) {
	t.Helper()
	out := got{}
	err := c.Collect(context.Background(), func(m collector.Metric) { out[m.Name] = m })
	return out, err
}

func mustBuild(t *testing.T, cfg Config, src Source, fc *testutil.FakeClock) *Check {
	t.Helper()
	c, err := build(cfg, src, fc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProcess_SumsOverMatches(t *testing.T) {
	src := machine()
	fc := testutil.NewFakeClock(t0)
	c := mustBuild(t, Config{ProcessName: "nginx"}, src, fc)
	g, err := run(t, c)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{
		"system.processes.number": 2, "system.processes.mem.rss": 4000,
		"system.processes.threads": 3, "system.processes.open_file_descriptors": 30,
	} {
		if m, ok := g[name]; !ok || m.Value != want {
			t.Errorf("%s = %+v, want %v", name, m, want)
		}
	}
	if g["system.processes.number"].Tags[0] != "process_name:nginx" {
		t.Errorf("tags = %v", g["system.processes.number"].Tags)
	}
	if _, ok := g["system.processes.cpu.pct"]; ok {
		t.Error("cpu from one reading")
	}

	// 10s later: the master used 1s more CPU, the worker 4s more (10% + 40%),
	// and a new worker appeared, which counts but adds no CPU yet.
	fc.Advance(10 * time.Second)
	src.usage[10] = Usage{CPUSeconds: 2, RSS: 1000, Started: 1, FDsOK: true}
	src.usage[11] = Usage{CPUSeconds: 9, RSS: 3000, Started: 2, FDsOK: true}
	src.usage[12] = Usage{CPUSeconds: 100, RSS: 1, Started: 5, FDsOK: true}
	g, _ = run(t, c)
	if v := g["system.processes.cpu.pct"].Value; v != 50 {
		t.Errorf("cpu.pct = %v, want 50", v)
	}
	if g["system.processes.number"].Value != 3 {
		t.Errorf("number = %v", g["system.processes.number"].Value)
	}

	// The master exits and the kernel hands its pid to an unrelated
	// process: a new start time, so no rate against the old reading.
	fc.Advance(10 * time.Second)
	src.usage[10] = Usage{CPUSeconds: 500, Started: 99}
	src.usage[11] = Usage{CPUSeconds: 10, Started: 2}
	src.usage[12] = Usage{CPUSeconds: 100, Started: 5}
	g, _ = run(t, c)
	if v := g["system.processes.cpu.pct"].Value; v != 10 {
		t.Errorf("cpu.pct = %v, want 10: the reused pid was differenced", v)
	}
	if _, ok := g["system.processes.open_file_descriptors"]; ok {
		t.Error("a partial fd sum was reported")
	}
}

func TestProcess_Matching(t *testing.T) {
	no := false
	for name, tc := range map[string]struct {
		cfg  Config
		want float64
	}{
		"exact name":               {Config{ProcessName: "python3"}, 1},
		"exact is not a substring": {Config{ProcessName: "python"}, 0},
		"command line":             {Config{ProcessName: "uvicorn", ExactMatch: &no}, 1},
		"pattern":                  {Config{Pattern: `-D /data$`, Label: "pg"}, 1},
		"pattern, none":            {Config{Pattern: `^redis`, Label: "redis"}, 0},
	} {
		g, err := run(t, mustBuild(t, tc.cfg, machine(), testutil.NewFakeClock(t0)))
		if err != nil || g["system.processes.number"].Value != tc.want {
			t.Errorf("%s: %v %v", name, g, err)
		}
		if tc.want == 0 && len(g) != 1 {
			t.Errorf("%s: no matches should report only the count: %v", name, g)
		}
	}
}

func TestProcess_PIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg.pid")
	c := mustBuild(t, Config{PIDFile: path, Label: "pg"}, machine(), testutil.NewFakeClock(t0))

	g, err := run(t, c) // no file: not running
	if err != nil || g["system.processes.number"].Value != 0 {
		t.Fatalf("%v %v", g, err)
	}
	_ = os.WriteFile(path, []byte("30\n"), 0o600)
	g, err = run(t, c)
	if err != nil || g["system.processes.number"].Value != 1 || g["system.processes.mem.rss"].Value != 800 {
		t.Fatalf("%v %v", g, err)
	}
	_ = os.WriteFile(path, []byte("thirty"), 0o600)
	if _, err = run(t, c); err == nil || !strings.Contains(err.Error(), "not a pid") {
		t.Fatalf("err = %v", err)
	}
	c = mustBuild(t, Config{PIDFile: dir, Label: "pg"}, machine(), testutil.NewFakeClock(t0))
	if _, err = run(t, c); err == nil {
		t.Fatal("reading a directory as a pid file: no error")
	}
}

func TestProcess_ListFails(t *testing.T) {
	src := machine()
	src.listErr = errors.New("proc unreadable")
	if _, err := run(t, mustBuild(t, Config{ProcessName: "nginx"}, src, testutil.NewFakeClock(t0))); err == nil {
		t.Fatal("no error")
	}
}

func TestNew_Refuses(t *testing.T) {
	yes := true
	for name, tc := range map[string]struct {
		settings map[string]any
		want     string
	}{
		"nothing":            {map[string]any{}, "exactly one"},
		"two selectors":      {map[string]any{"process_name": "a", "pattern": "b"}, "exactly one"},
		"bad pattern":        {map[string]any{"pattern": "(", "label": "x"}, "pattern"},
		"pattern, no label":  {map[string]any{"pattern": "x"}, "label is required"},
		"pid file, no label": {map[string]any{"pid_file": "/run/x.pid"}, "label is required"},
		"untaggable name":    {map[string]any{"process_name": "a,b"}, "cannot be a tag"},
		"unknown setting":    {map[string]any{"label_x": "x"}, "label_x"},
	} {
		_, err := collector.Registry{Name: New}.NewInstance(Name, 0, 1, tc.settings, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if _, err := build(Config{Pattern: "x", Label: "x", ExactMatch: &yes}, machine(), nil); err == nil {
		t.Error("exact_match with pattern accepted")
	}
}

// The real process table: this test's own process is found by pid file,
// with plausible numbers.
func TestSystem_ThisProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "self.pid")
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	c, err := New(collector.Instance{Name: Name, Settings: map[string]any{"pid_file": path, "label": "self"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != Name || c.Interval() != 0 {
		t.Errorf("%q %v", c.Name(), c.Interval())
	}
	g, err := run(t, c)
	if err != nil || g["system.processes.number"].Value != 1 || g["system.processes.mem.rss"].Value <= 0 || g["system.processes.threads"].Value < 1 {
		t.Fatalf("%v %v", g, err)
	}
	procs, err := System{}.List(context.Background())
	if err != nil || len(procs) == 0 {
		t.Fatalf("list: %d processes, %v", len(procs), err)
	}
	if _, err := (System{}).Usage(context.Background(), 1<<30); err == nil {
		t.Error("usage of a pid that cannot exist: no error")
	}
}

// `name` is the instance's name, as for every check; process_name selects.
func TestNew_InstanceNameAndProcessName(t *testing.T) {
	c, err := collector.Registry{Name: New}.NewInstance(Name, 0, 1, map[string]any{"name": "web", "process_name": "nginx"}, nil, nil, nil)
	if err != nil || c.Name() != "process:web" {
		t.Fatalf("%v %v", c, err)
	}
}
