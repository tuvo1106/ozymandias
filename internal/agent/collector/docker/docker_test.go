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
	onInspect func(id string) // called during Inspect, without f.mu held
	onStats   func(id string) // called during Stats, without f.mu held

	// events: each Events call takes the next script entry, delivers its
	// events, then returns its error.
	scripts [][]dockerapi.Event
	errs    []error
	sinces  []time.Time
	skips   int // lines each Events call reports as undecodable first
	// daemonNow and nowErr are what Now answers.
	daemonNow time.Time
	nowErr    error
	// onEvents, if set, is called at the start of each Events call with
	// its index, without f.mu held.
	onEvents func(call int)
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
	if f.onStats != nil {
		f.onStats(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statsErr[id]; err != nil {
		return dockerapi.Stats{}, err
	}
	return f.stats[id], nil
}

func (f *fakeAPI) Inspect(_ context.Context, id string) (dockerapi.ContainerJSON, error) {
	if f.onInspect != nil {
		f.onInspect(id)
	}
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

func (f *fakeAPI) Events(ctx context.Context, since time.Time, fn func(dockerapi.Event) error, skipped func(error)) error {
	if f.onEvents != nil {
		f.mu.Lock()
		n := len(f.sinces)
		f.mu.Unlock()
		f.onEvents(n)
	}
	f.mu.Lock()
	f.sinces = append(f.sinces, since)
	for range f.skips {
		skipped(errors.New("undecodable line"))
	}
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

func (f *fakeAPI) Now(context.Context) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.daemonNow, f.nowErr
}

// stats builds a sampled stats answer with cumulative counters scaled by n.
func stats(n uint64) dockerapi.Stats {
	var s dockerapi.Stats
	s.Read = t0.Add(time.Duration(n-1) * 1500 * time.Millisecond) // stats(1) is read at t0; 10 steps of n are the tests' 15s
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
	return dockerapi.Container{ID: id, Names: []string{"/" + name}, Image: image, Labels: labels}
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
	c := New(Options{API: api})
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
		"container.memory.usage": 900, // minus inactive file cache
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
	for _, rate := range []string{"container.cpu.usage", "container.net.rx_bytes", "container.io.read_bytes", "container.cpu.throttled"} {
		if len(g[rate]) != 0 {
			t.Errorf("%s on the first run: it needs two readings", rate)
		}
	}
	// One-shot stats carry no previous sample, so CPU is measured between
	// this run's reading and the last one.
	api.mu.Lock()
	api.stats[idAPI] = stats(2)
	api.mu.Unlock()
	g, err = collect(t, c)
	if err != nil {
		t.Fatal(err)
	}
	// (250-150) / (21000-11000) × 4 cpus × 100
	if v := g.one(t, "container.cpu.usage").Value; math.Abs(v-4) > 1e-9 {
		t.Errorf("cpu = %v, want 4", v)
	}
}

func TestDocker_RatesOnTheSecondRun(t *testing.T) {
	api := &fakeAPI{
		list:  []dockerapi.Container{ctr(idAPI, "api", "api", nil)},
		stats: map[string]dockerapi.Stats{idAPI: stats(1)},
	}
	c := New(Options{API: api})
	_, _ = collect(t, c)
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
	c := New(Options{API: api, Rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-.*`), Replace: "judge"}}})
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
	c := New(Options{API: api})
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
	_, err := collect(t, New(Options{API: api}))
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
	_, _ = collect(t, New(Options{API: api, MaxConcurrency: 4}))
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
	c := New(Options{API: api})
	for range 3 {
		_, _ = collect(t, c)
	}
	if n := api.inspected[idAPI]; n != 1 {
		t.Fatalf("inspected %d times", n)
	}
	api.list = nil
	_, _ = collect(t, c)
	if len(c.lives) != 0 || len(c.seen) != 0 {
		t.Fatalf("still tracking %v, %v", c.lives, c.seen)
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

// A rule replaces the whole name, not only the part it matched: a prefix
// rule folds every sandbox into one name rather than leaving each unique
// (and, without its id, unidentifiable).
func TestTags_ARewriteReplacesTheWholeName(t *testing.T) {
	tg := tagger{rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-`), Replace: "judge"}}}
	for _, name := range []string{"judge-8f3a", "judge-py-1"} {
		if got := sortedTags(tg.tags(name, idAPI, "x", nil)); got != "container_name:judge,image_name:x,image_tag:latest" {
			t.Errorf("%s: %s", name, got)
		}
	}
}

// A replacement that expands to nothing keeps the container's own name and
// id rather than leaving it with neither.
func TestTags_AnEmptyExpansionKeepsTheName(t *testing.T) {
	tg := tagger{rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-(\w*)`), Replace: "${2}"}}}
	if got := sortedTags(tg.tags("judge-1", idAPI, "x", nil)); got != "container_id:aaaaaaaaaaaa,container_name:judge-1,image_name:x,image_tag:latest" {
		t.Fatalf("got %s", got)
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

// A container restarted in place keeps its id; its uptime starts again.
func TestDocker_AStartForgetsTheCachedStartTime(t *testing.T) {
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "api", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Hour)}}},
	}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	api.mu.Lock()
	api.inspect[idAPI] = dockerapi.ContainerJSON{State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Minute)}}
	api.mu.Unlock()
	c.ContainerStarted(idAPI)
	g, _ := collect(t, c)
	if v := g.one(t, "container.uptime").Value; v != 60 {
		t.Fatalf("uptime = %v, want 60: the restart's start time", v)
	}
}

// An image whose last container stopped reports 0 once, then nothing.
func TestDocker_AVanishedImageReportsZeroOnce(t *testing.T) {
	api := &fakeAPI{
		list:  []dockerapi.Container{ctr(idAPI, "api", "api:1", nil)},
		stats: map[string]dockerapi.Stats{idAPI: stats(1)},
	}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	api.mu.Lock()
	api.list = nil
	api.mu.Unlock()
	g, _ := collect(t, c)
	if r := g.one(t, "docker.containers.running"); r.Value != 0 || r.Tags[0] != "image_name:api" {
		t.Fatalf("running = %+v, want 0 for api", r)
	}
	if g, _ = collect(t, c); len(g["docker.containers.running"]) != 0 {
		t.Fatalf("still reporting %+v", g["docker.containers.running"])
	}
}

// A container with no network (--network none) or no block-io entries has
// no such counters: unknown, not a rate of 0.
func TestDocker_MissingCountersAreNotZero(t *testing.T) {
	st := func(n uint64) dockerapi.Stats {
		s := stats(n)
		s.Networks, s.BlkioStats.IoServiceBytesRecursive = nil, nil
		return s
	}
	api := &fakeAPI{list: []dockerapi.Container{ctr(idAPI, "api", "x", nil)}, stats: map[string]dockerapi.Stats{idAPI: st(1)}}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	api.mu.Lock()
	api.stats[idAPI] = st(2)
	api.mu.Unlock()
	g, _ := collect(t, c)
	for _, name := range []string{"container.net.rx_bytes", "container.net.tx_bytes", "container.io.read_bytes", "container.io.write_bytes"} {
		if len(g[name]) != 0 {
			t.Errorf("%s = %+v, want nothing", name, g[name])
		}
	}
	g.one(t, "container.cpu.throttled")
}

// Two containers whose tags differ only in case are one series after
// normalization, so they are combined, not sent as two points for one
// series (the last would win).
func TestDocker_TagsThatNormalizeAlikeAreCombined(t *testing.T) {
	other := "b" + idAPI[1:]
	api := &fakeAPI{
		list:  []dockerapi.Container{ctr(idAPI, "Job", "x", nil), ctr(other, "job", "x", nil)},
		stats: map[string]dockerapi.Stats{idAPI: stats(1), other: stats(1)},
	}
	c := New(Options{API: api, Rewrites: []Rewrite{{Match: regexp.MustCompile(`(?i)^job$`), Replace: "${0}"}}})
	g, _ := collect(t, c)
	if m := g.one(t, "container.memory.usage"); m.Value != 1800 {
		t.Fatalf("memory = %v, want 2 × 900", m.Value)
	}
}

// A restart that lands while its container is being inspected: the answer
// may be the old start, so it is not cached, and the next run asks again.
func TestDocker_ARestartDuringInspectIsNotCached(t *testing.T) {
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "api", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Hour)}}},
	}
	c := New(Options{API: api})
	api.onInspect = func(id string) {
		api.onInspect = nil // once
		c.ContainerStarted(id)
		api.mu.Lock()
		api.inspect[id] = dockerapi.ContainerJSON{State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Minute)}}
		api.mu.Unlock()
	}
	g, _ := collect(t, c)
	if len(g["container.uptime"]) != 0 {
		t.Errorf("uptime %+v from an inspect a restart overtook", g["container.uptime"])
	}
	g, _ = collect(t, c)
	if v := g.one(t, "container.uptime").Value; v != 60 {
		t.Fatalf("uptime = %v, want 60 from the restart", v)
	}
}

// A run that runs out of time before reaching a container must not forget
// that container's previous sample and start time: it is still running.
func TestDocker_ATimedOutRunKeepsWhatItDidNotReach(t *testing.T) {
	other := "b" + idAPI[1:]
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "a", "x", nil), ctr(other, "b", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1), other: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0}}, other: {State: dockerapi.ContainerState{StartedAt: t0}}},
	}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Collect(ctx, func(collector.Metric) {})
	if len(c.seen) != 2 || len(c.lives) != 2 {
		t.Fatalf("seen %d, lives %d after a timed-out run; want both kept", len(c.seen), len(c.lives))
	}
}

func TestDocker_NoPidsStatsIsNotZeroPids(t *testing.T) {
	st := stats(1)
	st.PidsStats.Current = 0
	api := &fakeAPI{list: []dockerapi.Container{ctr(idAPI, "api", "x", nil)}, stats: map[string]dockerapi.Stats{idAPI: st}}
	g, _ := collect(t, New(Options{API: api}))
	if len(g["container.pids"]) != 0 {
		t.Fatalf("pids = %+v, want nothing", g["container.pids"])
	}
}

// A rate is timed by the daemon's sample, not the run: a container queued
// behind others is sampled later than the run began, by a different amount
// each run.
func TestDocker_RatesAreTimedByTheSample(t *testing.T) {
	first, second := stats(1), stats(11)
	first.Read, second.Read = t0, t0.Add(20*time.Second) // not the 15s the fixture would say
	api := &fakeAPI{list: []dockerapi.Container{ctr(idAPI, "api", "x", nil)}, stats: map[string]dockerapi.Stats{idAPI: first}}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	api.mu.Lock()
	api.stats[idAPI] = second
	api.mu.Unlock()
	g, _ := collect(t, c)
	if v := g.one(t, "container.net.rx_bytes").Value; v != 750 {
		t.Fatalf("rx = %v, want 15000 bytes / 20s = 750", v)
	}
}

// The daemon's clock need not agree with the agent's (a VM's clock after
// the host sleeps). Rates are timed on the daemon's, so their readings must
// not be pruned by the agent's: an hour of skew still gives rates, and
// uptime is measured on the daemon's clock too.
func TestDocker_AnHourOfClockSkew(t *testing.T) {
	skew := -time.Hour
	st := func(n uint64) dockerapi.Stats {
		s := stats(n)
		s.Read = s.Read.Add(skew)
		return s
	}
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "api", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: st(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0.Add(skew - time.Minute)}}},
	}
	c := New(Options{API: api})
	g, _ := collect(t, c)
	if v := g.one(t, "container.uptime").Value; v != 60 {
		t.Errorf("uptime = %v, want 60 on the daemon's clock", v)
	}
	api.mu.Lock()
	api.stats[idAPI] = st(11)
	api.mu.Unlock()
	g, _ = collect(t, c)
	if v := g.one(t, "container.net.rx_bytes").Value; v != 1000 {
		t.Fatalf("rx = %v, want 1000", v)
	}
}

// A container restarted in place keeps its id, but its counters begin
// again: the run after the restart differences nothing, rather than the new
// life's counters against the old life's.
func TestDocker_ARestartDropsTheBaselines(t *testing.T) {
	api := &fakeAPI{list: []dockerapi.Container{ctr(idAPI, "api", "x", nil)}, stats: map[string]dockerapi.Stats{idAPI: stats(1)}}
	c := New(Options{API: api})
	next := func(n uint64) got {
		api.mu.Lock()
		api.stats[idAPI] = stats(n)
		api.mu.Unlock()
		g, _ := collect(t, c)
		return g
	}
	_, _ = collect(t, c)
	c.ContainerStarted(idAPI)
	g := next(11) // counters grew past the old life's: no check would catch it
	for _, name := range []string{"container.cpu.usage", "container.net.rx_bytes", "container.cpu.throttled"} {
		if len(g[name]) != 0 {
			t.Errorf("%s = %+v across a restart", name, g[name])
		}
	}
	g = next(21)
	g.one(t, "container.cpu.usage")
	g.one(t, "container.net.rx_bytes")
}

// A start event that lands while a container's stats are read: the sample
// may be from either life, so the run reports nothing for it — not an
// uptime from the old start against the new life's clock.
func TestDocker_ARestartDuringStatsSkipsTheSample(t *testing.T) {
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "api", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Hour)}}},
	}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	api.mu.Lock()
	api.stats[idAPI] = stats(11)
	api.inspect[idAPI] = dockerapi.ContainerJSON{State: dockerapi.ContainerState{StartedAt: t0.Add(10 * time.Second)}}
	api.mu.Unlock()
	api.onStats = func(id string) {
		api.onStats = nil
		c.ContainerStarted(id)
	}
	g, _ := collect(t, c)
	for _, name := range []string{"container.uptime", "container.cpu.usage", "container.net.rx_bytes", "container.memory.usage"} {
		if len(g[name]) != 0 {
			t.Errorf("%s = %+v from a sample taken across a restart", name, g[name])
		}
	}
	g, _ = collect(t, c)
	if v := g.one(t, "container.uptime").Value; v != 5 {
		t.Errorf("uptime = %v, want 5: the new start, read at t0+15s", v)
	}
	if len(g["container.cpu.usage"]) != 0 {
		t.Errorf("cpu = %+v on the first sample of the new life", g["container.cpu.usage"])
	}
}

// The same failures make the same message, whatever order the calls end in:
// the scheduler logs a collector's error only when its text changes.
func TestSummarize_IsStable(t *testing.T) {
	a, b := errors.New("a: timeout"), errors.New("b: timeout")
	if x, y := summarize([]error{a, b}), summarize([]error{b, a}); x.Error() != y.Error() {
		t.Fatalf("%q != %q", x, y)
	}
}

// A restart the watcher never saw (the daemon restarted; its empty event
// buffer replays nothing): the CPU counter going backwards gives it away,
// and the container is treated as started — no sample across it, and the
// start time is read again.
func TestDocker_AnUnseenRestartIsCaughtByTheCounter(t *testing.T) {
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "api", "x", nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(11)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: {State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Hour)}}},
	}
	c := New(Options{API: api})
	_, _ = collect(t, c)
	api.mu.Lock()
	api.stats[idAPI] = stats(2) // counters began again
	api.inspect[idAPI] = dockerapi.ContainerJSON{State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Second)}}
	api.mu.Unlock()
	g, _ := collect(t, c)
	if len(g["container.uptime"]) != 0 || len(g["container.cpu.usage"]) != 0 {
		t.Fatalf("uptime %+v, cpu %+v from the sample that revealed the restart", g["container.uptime"], g["container.cpu.usage"])
	}
	api.mu.Lock()
	api.stats[idAPI] = stats(3)
	api.mu.Unlock()
	g, _ = collect(t, c)
	if v := g.one(t, "container.uptime").Value; v != 4 {
		t.Errorf("uptime = %v, want 4: the new start (t0-1s) to the sample (t0+3s)", v)
	}
}

// Tags follow a rename, and the image as the container was started from
// it (the inspect's), not the list's, which turns into the image id once
// the tag is re-pointed: the event watcher tags by the reference too.
func TestDocker_TagsFollowRenameAndTheStartedImage(t *testing.T) {
	j := dockerapi.ContainerJSON{State: dockerapi.ContainerState{StartedAt: t0}}
	j.Config.Image = "app:latest"
	api := &fakeAPI{
		list:    []dockerapi.Container{ctr(idAPI, "web", "sha256:"+strings.Repeat("b", 64), nil)},
		stats:   map[string]dockerapi.Stats{idAPI: stats(1)},
		inspect: map[string]dockerapi.ContainerJSON{idAPI: j},
	}
	c := New(Options{API: api})
	g, _ := collect(t, c)
	if m := g.one(t, "container.memory.usage"); !slices.Contains(m.Tags, "image_name:app") || !slices.Contains(m.Tags, "image_tag:latest") {
		t.Errorf("tags %v, want the started reference app:latest", m.Tags)
	}
	api.mu.Lock()
	api.list = []dockerapi.Container{ctr(idAPI, "web-old", "sha256:"+strings.Repeat("b", 64), nil)}
	api.mu.Unlock()
	g, _ = collect(t, c)
	if m := g.one(t, "container.memory.usage"); !slices.Contains(m.Tags, "container_name:web-old") {
		t.Errorf("tags %v after docker rename, want container_name:web-old", m.Tags)
	}
}
