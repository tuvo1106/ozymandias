package docker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var t0 = time.Unix(1_790_000_000, 0)

// fakeAPI is a scripted Docker daemon.
type fakeAPI struct {
	mu        sync.Mutex
	list      []dockerapi.Container
	listErr   error
	stats     map[string]dockerapi.Stats
	statsErr  map[string]error
	inspect   map[string]dockerapi.ContainerJSON
	inspected map[string]int
	inFlight  atomic.Int32
	maxFlight atomic.Int32
	statsWait time.Duration

	// events: each Events call takes the next script entry, delivers its
	// events, then returns its error.
	scripts [][]dockerapi.Event
	errs    []error
	sinces  []time.Time
}

func (f *fakeAPI) ListContainers(context.Context) ([]dockerapi.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list), f.listErr
}

func (f *fakeAPI) Stats(_ context.Context, id string) (dockerapi.Stats, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxFlight.Load()
		if n <= m || f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(f.statsWait)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statsErr[id]; err != nil {
		return dockerapi.Stats{}, err
	}
	return f.stats[id], nil
}

func (f *fakeAPI) Inspect(_ context.Context, id string) (dockerapi.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspected == nil {
		f.inspected = map[string]int{}
	}
	f.inspected[id]++
	j, ok := f.inspect[id]
	if !ok {
		return j, &dockerapi.APIError{Status: 404, Message: "no such container"}
	}
	return j, nil
}

func (f *fakeAPI) Events(ctx context.Context, since time.Time, fn func(dockerapi.Event) error) error {
	f.mu.Lock()
	f.sinces = append(f.sinces, since)
	if len(f.scripts) == 0 {
		f.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	evs, err := f.scripts[0], f.errs[0]
	f.scripts, f.errs = f.scripts[1:], f.errs[1:]
	f.mu.Unlock()
	for _, ev := range evs {
		if e := fn(ev); e != nil {
			return e
		}
	}
	return err
}

// stats builds a sampled stats answer with cumulative counters scaled by n.
func stats(n uint64) dockerapi.Stats {
	var s dockerapi.Stats
	s.Read = t0
	s.PreCPUStats.CPUUsage.TotalUsage = 100 * n
	s.PreCPUStats.SystemUsage = 10_000 * n
	s.CPUStats.CPUUsage.TotalUsage = 100*n + 50
	s.CPUStats.SystemUsage = 10_000*n + 1000
	s.CPUStats.OnlineCPUs = 4
	s.CPUStats.ThrottlingData.ThrottledPeriods = 3 * n
	s.MemoryStats = dockerapi.MemoryStats{Usage: 1000, Limit: 4000, Stats: map[string]uint64{"anon": 600, "file": 300, "inactive_file": 100}}
	s.Networks = map[string]dockerapi.NetworkStats{"eth0": {RxBytes: 1500 * n, TxBytes: 150 * n}}
	s.BlkioStats.IoServiceBytesRecursive = []dockerapi.BlkioEntry{{Op: "read", Value: 3000 * n}, {Op: "write", Value: 30 * n}, {Op: "total", Value: 1 << 40}}
	s.PidsStats.Current = 7
	return s
}

func ctr(id, name, image string, labels map[string]string) dockerapi.Container {
	return dockerapi.Container{ID: id, Names: []string{"/" + name}, Image: image, Labels: labels, State: "running"}
}

type got map[string][]collector.Metric

func collect(t *testing.T, c *Collector) (got, error) {
	t.Helper()
	out := got{}
	var mu sync.Mutex
	err := c.Collect(context.Background(), func(m collector.Metric) {
		mu.Lock()
		defer mu.Unlock()
		out[m.Name] = append(out[m.Name], m)
	})
	return out, err
}

func (g got) one(t *testing.T, name string) collector.Metric {
	t.Helper()
	if len(g[name]) != 1 {
		t.Fatalf("%s: %d metrics, want 1: %+v", name, len(g[name]), g[name])
	}
	return g[name][0]
}

const idAPI = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"

func compose() map[string]string {
	return map[string]string{LabelComposeProject: "shop", LabelComposeService: "api"}
}

func TestDocker_TagsAndGauges(t *testing.T) {
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "shop-api-1", "ghcr.io/acme/api:1.4", compose())},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Hour)}}},
	}
	c := New(Options{API: api, Clock: testutil.NewFakeClock(t0)})
	g, err := collect(t, c)
	if err != nil {
		t.Fatal(err)
	}
	m := g.one(t, "container.memory.usage")
	want := "compose_project:shop,compose_service:api,container_id:aaaaaaaaaaaa,container_name:shop-api-1,image_name:ghcr.io/acme/api,image_tag:1.4,service:shop-api"
	if sortedTags(m.Tags) != want {
		t.Errorf("tags = %s\nwant   %s", sortedTags(m.Tags), want)
	}
	for name, w := range map[string]float64{
		"container.cpu.usage":    50.0 / 1000 * 4 * 100, // cpu_delta / system_delta × cpus × 100
		"container.memory.usage": 900,                   // minus inactive file cache
		"container.memory.limit": 4000,
		"container.memory.rss":   600,
		"container.memory.cache": 300,
		"container.pids":         7,
		"container.uptime":       3600,
	} {
		if v := g.one(t, name).Value; math.Abs(v-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, v, w)
		}
	}
	if r := g.one(t, "docker.containers.running"); r.Value != 1 || r.Tags[0] != "image_name:ghcr.io/acme/api" {
		t.Errorf("running = %+v", r)
	}
	for _, rate := range []string{"container.net.rx_bytes", "container.io.read_bytes", "container.cpu.throttled"} {
		if len(g[rate]) != 0 {
			t.Errorf("%s on the first run: a rate needs two readings", rate)
		}
	}
}

func TestDocker_RatesOnTheSecondRun(t *testing.T) {
	api := &fakeAPI{
		list:  []dockerapi.Container{ctr(idAPI, "api", "api", nil)},
		stats: map[string]dockerapi.Stats{idAPI: stats(1)},
	}
	fc := testutil.NewFakeClock(t0)
	c := New(Options{API: api, Clock: fc})
	_, _ = collect(t, c)
	fc.Advance(15 * time.Second)
	api.stats[idAPI] = stats(11) // +10 units of everything
	g, _ := collect(t, c)
	for name, w := range map[string]float64{
		"container.net.rx_bytes":   1000, // 15000 bytes / 15s
		"container.net.tx_bytes":   100,
		"container.io.read_bytes":  2000, // "total" is not added in
		"container.io.write_bytes": 20,
		"container.cpu.throttled":  2,
	} {
		m := g.one(t, name)
		if m.Kind != collector.Rate || math.Abs(m.Value-w) > 1e-9 {
			t.Errorf("%s = %v (%v), want rate %v", name, m.Value, m.Kind, w)
		}
	}
}

// Judge sandboxes: many, short-lived, one name after the rewrite. They are
// one series, with amounts summed, and no container_id to split them again.
func TestDocker_RewrittenContainersAreOneSeries(t *testing.T) {
	var list []dockerapi.Container
	st := map[string]dockerapi.Stats{}
	insp := map[string]dockerapi.ContainerJSON{}
	for i := range 3 {
		id := fmt.Sprintf("%064d", i+1)
		list = append(list, ctr(id, fmt.Sprintf("judge-%d-py", i), "judge:py", nil))
		st[id] = stats(1)
		insp[id] = dockerapi.ContainerJSON{State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Duration(i+1) * time.Minute)}}
	}
	api := &fakeAPI{list: list, stats: st, inspect: insp}
	c := New(Options{API: api, Clock: testutil.NewFakeClock(t0), Rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-.*`), Replace: "judge"}}})
	g, _ := collect(t, c)
	m := g.one(t, "container.memory.usage")
	if m.Value != 2700 {
		t.Errorf("memory = %v, want 3 × 900", m.Value)
	}
	if strings.Contains(sortedTags(m.Tags), "container_id") || !slices.Contains(m.Tags, "container_name:judge") {
		t.Errorf("tags = %v", m.Tags)
	}
	if v := g.one(t, "container.uptime").Value; v != 180 {
		t.Errorf("uptime = %v, want the oldest (180s), not a sum", v)
	}
	if v := g.one(t, "container.memory.limit").Value; v != 4000 {
		t.Errorf("limit = %v, want 4000: limits do not add up", v)
	}
	if v := g.one(t, "docker.containers.running").Value; v != 3 {
		t.Errorf("running = %v", v)
	}
}

// The normal race: a container stops between the list and its stats call.
func TestDocker_AContainerThatStopsIsNotAnError(t *testing.T) {
	gone, stopped, broken := "b"+idAPI[1:], "c"+idAPI[1:], "d"+idAPI[1:]
	api := &fakeAPI{
		list:  []dockerapi.Container{ctr(idAPI, "ok", "x", nil), ctr(gone, "gone", "x", nil), ctr(stopped, "stopped", "x", nil), ctr(broken, "broken", "x", nil)},
		stats: map[string]dockerapi.Stats{idAPI: stats(1), stopped: {}},
		statsErr: map[string]error{
			gone:   &dockerapi.APIError{Status: 404},
			broken: errors.New("daemon hiccup"),
		},
	}
	c := New(Options{API: api, Clock: testutil.NewFakeClock(t0)})
	g, err := collect(t, c)
	if err == nil || !strings.Contains(err.Error(), "broken: daemon hiccup") || strings.Contains(err.Error(), "gone") {
		t.Fatalf("err = %v, want only the broken one", err)
	}
	if len(g["container.pids"]) != 1 {
		t.Errorf("pids from %d containers, want 1", len(g["container.pids"]))
	}
}

func TestDocker_ManyFailuresMakeOneBoundedError(t *testing.T) {
	api := &fakeAPI{statsErr: map[string]error{}}
	for i := range 20 {
		id := fmt.Sprintf("%064d", i+1)
		api.list = append(api.list, ctr(id, fmt.Sprintf("c%d", i), "x", nil))
		api.statsErr[id] = errors.New("overloaded")
	}
	_, err := collect(t, New(Options{API: api, Clock: testutil.NewFakeClock(t0)}))
	if err == nil || !strings.Contains(err.Error(), "and 19 more containers") {
		t.Fatalf("err = %v", err)
	}
}

func TestDocker_ListFailureIsTheRunsError(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("dial unix /var/run/docker.sock: no such file")}
	if _, err := collect(t, New(Options{API: api})); err == nil {
		t.Fatal("no error")
	}
}

func TestDocker_BoundedConcurrency(t *testing.T) {
	api := &fakeAPI{stats: map[string]dockerapi.Stats{}, statsWait: 5 * time.Millisecond}
	for i := range 30 {
		id := fmt.Sprintf("%064d", i+1)
		api.list = append(api.list, ctr(id, fmt.Sprintf("c%d", i), "x", nil))
		api.stats[id] = stats(1)
	}
	_, _ = collect(t, New(Options{API: api, MaxConcurrency: 4, Clock: testutil.NewFakeClock(t0)}))
	if m := api.maxFlight.Load(); m > 4 || m < 2 {
		t.Fatalf("max in flight = %d, want 2..4", m)
	}
}

// A container is inspected once, not every run, and forgotten when gone.
func TestDocker_StartTimesAreCachedAndForgotten(t *testing.T) {
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "api", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0}}},
	}
	c := New(Options{API: api, Clock: testutil.NewFakeClock(t0)})
	for range 3 {
		_, _ = collect(t, c)
	}
	if n := api.inspected[idAPI]; n != 1 {
		t.Fatalf("inspected %d times", n)
	}
	api.list = nil
	_, _ = collect(t, c)
	if len(c.started) != 0 {
		t.Fatalf("still tracking %v", c.started)
	}
}

func TestTags(t *testing.T) {
	tg := tagger{rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-(\w+)-.*$`), Replace: "judge-$1"}}}
	for _, tc := range []struct {
		name, image string
		labels      map[string]string
		want        string
	}{
		{"web", "nginx", nil, "container_id:aaaaaaaaaaaa,container_name:web,image_name:nginx,image_tag:latest"},
		{"db", "postgres@sha256:" + strings.Repeat("a", 64), nil, "container_id:aaaaaaaaaaaa,container_name:db,image_name:postgres"},
		{"x", "img:1", map[string]string{LabelService: "billing", LabelComposeProject: "p", LabelComposeService: "s"},
			"compose_project:p,compose_service:s,container_id:aaaaaaaaaaaa,container_name:x,image_name:img,image_tag:1,service:billing"},
		{"x", "img:1", map[string]string{LabelComposeProject: "p"}, "compose_project:p,container_id:aaaaaaaaaaaa,container_name:x,image_name:img,image_tag:1"},
		{"judge-py-7f3a", "judge:py", nil, "container_name:judge-py,image_name:judge,image_tag:py"},
	} {
		if got := sortedTags(tg.tags(tc.name, idAPI, tc.image, tc.labels)); got != tc.want {
			t.Errorf("%s %s: %s\nwant %s", tc.name, tc.image, got, tc.want)
		}
	}
}

func TestDocker_NameAndInterval(t *testing.T) {
	c := New(Options{Interval: 30 * time.Second})
	if c.Name() != "docker" || c.Interval() != 30*time.Second || c.conc != DefaultMaxConcurrency {
		t.Fatalf("%q %v %d", c.Name(), c.Interval(), c.conc)
	}
}

// sortedTags is a stable rendering of a tag set.
func sortedTags(tags []string) string {
	t := slices.Clone(tags)
	slices.Sort(t)
	return strings.Join(t, ",")
}
