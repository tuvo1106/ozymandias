package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testConfig binds statsd to a free loopback port and points the forwarder
// at a port where nothing listens, so a test never collides with a running
// agent or sends its data to a real ozyd.
func testConfig() config.Agent {
	cfg := config.Default()
	cfg.HTTP.ShutdownTimeout = 2 * time.Second
	cfg.Statsd.Addr = "127.0.0.1:0"
	cfg.Intake.URL = "http://127.0.0.1:1"
	cfg.Forwarder.ShutdownTimeout = time.Second
	// Off: it reads the real machine, and its start-up timer would count
	// among the fake clock's waiters that tests wait on. Collectors have
	// their own test below.
	cfg.Collectors.Host.Enabled = false
	cfg.Collectors.Docker.Enabled = false
	return cfg
}

// newAgent is New plus cleanup of the statsd socket.
func newAgent(t *testing.T, cfg config.Agent, opts Options) (*Agent, error) {
	t.Helper()
	a, err := New(cfg, opts)
	if a != nil {
		t.Cleanup(func() { _ = a.Close() })
	}
	return a, err
}

func TestNew_ExplicitHostnameWins(t *testing.T) {
	cfg := testConfig()
	cfg.Hostname = "configured"
	a, err := newAgent(t, cfg, Options{Logger: quiet, Hostname: func() (string, error) { return "os", nil }})
	if err != nil || a.Hostname() != "configured" {
		t.Fatalf("hostname = %q, %v", a.Hostname(), err)
	}
}

func TestNew_FallsBackToOSHostname(t *testing.T) {
	a, err := newAgent(t, testConfig(), Options{Logger: quiet, Hostname: func() (string, error) { return "os-host", nil }})
	if err != nil || a.Hostname() != "os-host" {
		t.Fatalf("hostname = %q, %v", a.Hostname(), err)
	}
}

// Without a host tag every metric would be ambiguous across machines; refuse
// to start rather than send untagged data.
func TestNew_FailsWhenNoHostnameCanBeFound(t *testing.T) {
	_, err := newAgent(t, testConfig(), Options{Hostname: func() (string, error) { return "", errors.New("no uts") }})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("err = %v", err)
	}
}

func TestHandler_HealthzIncludesHostnameAndIntake(t *testing.T) {
	cfg := testConfig()
	cfg.Hostname = "box"
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["component"] != "agent" || body["hostname"] != "box" || body["intake_url"] != cfg.Intake.URL {
		t.Fatalf("healthz = %v", body)
	}
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/debug/vars", nil))
	if !strings.Contains(rec.Body.String(), `"component:agent"`) {
		t.Fatalf("/debug/vars = %s", rec.Body)
	}
}

func TestRun_ServesUntilCancelledThenStopsCleanly(t *testing.T) {
	testutil.CheckGoroutines(t)
	a, err := newAgent(t, testConfig(), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	if err := httpserve.Probe(context.Background(), "http://"+ln.Addr().String()+"/healthz", 2*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestRun_BindsConfiguredAddressOrReportsFailure(t *testing.T) {
	cfg := testConfig()
	cfg.HTTP.Addr = "127.0.0.1:0"
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx, nil); err != nil {
		t.Fatalf("Run = %v", err)
	}

	taken, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg.HTTP.Addr = taken.Addr().String()
	a, _ = newAgent(t, cfg, Options{Logger: quiet})
	if err := a.Run(context.Background(), nil); err == nil {
		t.Fatal("taken port: want error")
	}
}

func TestNew_DefaultsDependencies(t *testing.T) {
	a, err := newAgent(t, testConfig(), Options{})
	if err != nil || a.log == nil || a.reg == nil || a.clock == nil {
		t.Fatalf("defaults not applied: %v", err)
	}
}

func TestNew_StatsdPortConflictFailsStartup(t *testing.T) {
	a, err := newAgent(t, testConfig(), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Statsd.Addr = a.StatsdAddr().String()
	if _, err := newAgent(t, cfg, Options{Logger: quiet}); err == nil || !strings.Contains(err.Error(), "statsd listen") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_StatsdCanBeDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Statsd.Enabled = false
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil || a.StatsdAddr() != nil || a.Close() != nil {
		t.Fatalf("disabled statsd: addr=%v err=%v", a.StatsdAddr(), err)
	}
}

// The agent's whole pipeline in one process: a statsd datagram in, a gzip'd
// /v1/series POST out, carrying the value, the host tag, the agent's tags
// and the agent's own self-metrics.
func TestRun_StatsdToIntake(t *testing.T) {
	testutil.CheckGoroutines(t)
	var mu sync.Mutex
	var got []wire.Series
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var p wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&p)
		mu.Lock()
		got = append(got, p.Series...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer intake.Close()

	clk := testutil.NewFakeClock(time.Unix(1789999999, 0))
	cfg := testConfig()
	cfg.Hostname = "box"
	cfg.Tags = []string{"env:test"}
	cfg.Intake.URL = intake.URL
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // onto the boundary, a second after the agent started: its first whole bucket (aggregator.Options.Started)
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()

	conn, err := net.Dial("udp", a.StatsdAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("page.views:3|c|#route:/x\nqueue.depth:7|g\nusers:u1|s\nusers:u2|s\nlat:5|ms"))
	received := a.reg.Counter("ozy.agent.statsd.messages_received")
	testutil.Eventually(t, 2*time.Second, func() bool { return received.Value() == 5 }, "statsd got %d", received.Value())
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "aggregator not running")
	clk.Advance(10 * time.Second)

	find := func(metric string) *wire.Series {
		mu.Lock()
		defer mu.Unlock()
		for i := range got {
			if got[i].Metric == metric {
				return &got[i]
			}
		}
		return nil
	}
	testutil.Eventually(t, 3*time.Second, func() bool { return find("page.views") != nil }, "nothing forwarded")
	pv := find("page.views")
	if pv.Type != wire.KindCount || pv.Points[0] != (wire.Point{Timestamp: 1790000000, Value: 3}) ||
		strings.Join(pv.Tags, ",") != "env:test,host:box,route:/x" {
		t.Fatalf("page.views = %+v", pv)
	}
	for metric, want := range map[string]float64{"queue.depth": 7, "users": 2, "lat.max": 5, "lat.count": 1} {
		if s := find(metric); s == nil || s.Points[0].Value != want {
			t.Errorf("%s = %+v, want %v", metric, s, want)
		}
	}
	if s := find("ozy.agent.statsd.messages_received"); s == nil || !slices.Contains(s.Tags, "host:box") {
		t.Errorf("self-metrics not forwarded with the host tag: %+v", s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// stubCollector emits one gauge per run.
type stubCollector struct{}

func (stubCollector) Name() string            { return "stub" }
func (stubCollector) Interval() time.Duration { return time.Second }
func (stubCollector) Collect(_ context.Context, emit collector.Emit) error {
	emit(collector.Metric{Name: "stub.level", Kind: collector.Gauge, Value: 42, Tags: []string{"k:v"}})
	return nil
}

// Collector output reaches the intake directly, tagged like statsd series
// are, and the scheduler's own metrics ride along with the self-metrics.
func TestRun_CollectorsToIntake(t *testing.T) {
	testutil.CheckGoroutines(t)
	var mu sync.Mutex
	var got []wire.Series
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var p wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&p)
		mu.Lock()
		got = append(got, p.Series...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer intake.Close()

	clk := testutil.NewFakeClock(time.Unix(1790000001, 0))
	cfg := testConfig()
	cfg.Hostname = "box"
	cfg.Tags = []string{"env:test"}
	cfg.Intake.URL = intake.URL
	cfg.Statsd.Enabled = false
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: clk, Collectors: []collector.Collector{stubCollector{}}})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()

	// The aggregator's ticker and the collector's start-up timer.
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 2 }, "not armed")
	clk.Advance(time.Second)
	find := func(metric string) *wire.Series {
		mu.Lock()
		defer mu.Unlock()
		for i := range got {
			if got[i].Metric == metric {
				return &got[i]
			}
		}
		return nil
	}
	testutil.Eventually(t, 3*time.Second, func() bool { return find("stub.level") != nil }, "collector output not forwarded")
	s := find("stub.level")
	if s.Type != wire.KindGauge || s.Points[0].Value != 42 || strings.Join(s.Tags, ",") != "env:test,host:box,k:v" {
		t.Fatalf("stub.level = %+v", s)
	}
	// The next aggregator flush carries the scheduler's metrics.
	clk.Advance(10 * time.Second)
	testutil.Eventually(t, 3*time.Second, func() bool {
		r := find("ozy.agent.collector.runs")
		return r != nil && slices.Contains(r.Tags, "collector:stub")
	}, "collector self-metrics not forwarded")
	if find("ozy.runtime.goroutines") == nil {
		t.Error("runtime self-metrics not forwarded")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A listener that fails ends Run with its error, collectors or not: they
// must not wait for a SIGTERM that is not coming.
func TestRun_AListenerFailureStopsTheCollectors(t *testing.T) {
	testutil.CheckGoroutines(t)
	cfg := testConfig()
	cfg.Statsd.Enabled = false
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000001, 0)), Collectors: []collector.Collector{stubCollector{}}})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	_ = ln.Close()
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), ln) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a closed listener")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its listener failed")
	}
}

func TestNew_HostCollectorFollowsConfig(t *testing.T) {
	for _, on := range []bool{true, false} {
		cfg := testConfig()
		cfg.Collectors.Host.Enabled = on
		a, err := newAgent(t, cfg, Options{Logger: quiet})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range a.sched.Collectors() {
			names = append(names, c.Name())
		}
		if slices.Contains(names, "host") != on {
			t.Errorf("enabled=%v: collectors %v", on, names)
		}
	}
}

func TestNew_ABadInterfacePatternIsAnErrorNotAPanic(t *testing.T) {
	cfg := testConfig()
	cfg.Collectors.Host.Enabled = true
	cfg.Collectors.Host.ExcludeInterfaces = []string{"("}
	if _, err := newAgent(t, cfg, Options{Logger: quiet}); err == nil || !strings.Contains(err.Error(), "exclude_interfaces") {
		t.Fatalf("err = %v", err)
	}
}

// eventsAPI is a Docker daemon with no running containers whose event
// stream delivers evs once, then stays open until the agent stops.
type eventsAPI struct {
	evs       []dockerapi.Event
	delivered chan struct{}
	once      sync.Once
}

func (*eventsAPI) ListContainers(context.Context) ([]dockerapi.Container, error) { return nil, nil }
func (*eventsAPI) Stats(context.Context, string) (dockerapi.Stats, error) {
	return dockerapi.Stats{}, dockerapi.ErrNotFound
}
func (*eventsAPI) Inspect(context.Context, string) (dockerapi.ContainerJSON, error) {
	return dockerapi.ContainerJSON{}, dockerapi.ErrNotFound
}
func (e *eventsAPI) Now(context.Context) (time.Time, error) { return time.Time{}, nil }

func (e *eventsAPI) Events(ctx context.Context, _ time.Time, fn func(dockerapi.Event) error, _ func(error)) error {
	e.once.Do(func() {
		for _, ev := range e.evs {
			_ = fn(ev)
		}
		close(e.delivered)
	})
	<-ctx.Done()
	return ctx.Err()
}

// A container exit from the event stream reaches the intake as a count,
// through the aggregator, tagged like a statsd series and renamed by the
// configured rewrite.
func TestRun_DockerEventsToIntake(t *testing.T) {
	testutil.CheckGoroutines(t)
	var mu sync.Mutex
	var got []wire.Series
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var p wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&p)
		mu.Lock()
		got = append(got, p.Series...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer intake.Close()

	at := time.Unix(1790000000, 0)
	id := strings.Repeat("ab", 32)
	ev := func(action string, t time.Time, attrs map[string]string) dockerapi.Event {
		a := map[string]string{"name": "judge-42", "image": "sandbox:1"}
		maps.Copy(a, attrs)
		return dockerapi.Event{Type: "container", Action: action, Actor: dockerapi.Actor{ID: id, Attributes: a}, Time: t.Unix(), TimeNano: t.UnixNano()}
	}
	api := &eventsAPI{
		evs:       []dockerapi.Event{ev("start", at.Add(-3*time.Second), nil), ev("die", at, map[string]string{"exitCode": "3"})},
		delivered: make(chan struct{}),
	}
	clk := testutil.NewFakeClock(at.Add(-time.Second))
	cfg := testConfig()
	cfg.Hostname = "box"
	cfg.Tags = []string{"env:test"}
	cfg.Intake.URL = intake.URL
	cfg.Statsd.Enabled = false
	cfg.Collectors.Docker.Enabled = true
	cfg.Collectors.Docker.ContainerNameRewrite = []config.NameRewrite{{Match: `^judge-.*`, Replace: "judge"}}
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: clk, DockerAPI: api})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // onto the boundary, a second after the agent started: its first whole bucket (aggregator.Options.Started)
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()

	select {
	case <-api.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("event stream never read")
	}
	// The aggregator's ticker and the docker collector's start-up timer.
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 2 }, "not armed")
	clk.Advance(10 * time.Second)
	testutil.Eventually(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(got, func(s wire.Series) bool { return s.Metric == "container.exits" })
	}, "container.exits not forwarded")
	mu.Lock()
	i := slices.IndexFunc(got, func(s wire.Series) bool { return s.Metric == "container.exits" })
	s := got[i]
	mu.Unlock()
	if s.Type != wire.KindCount || s.Points[0].Value != 1 ||
		strings.Join(s.Tags, ",") != "container_name:judge,env:test,exit_code:3,host:box,image_name:sandbox,image_tag:1,oom_killed:false" {
		t.Fatalf("container.exits = %+v", s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The container-name cap is wired through the agent: with room for one name,
// the second unnamed container's exit reaches the intake as container_name:other,
// so the watcher and the cap really are connected. (The fold counter is not
// asserted here; namecap_test pins the callback.)
func TestRun_DockerEventsFoldNamesOverTheCap(t *testing.T) {
	testutil.CheckGoroutines(t)
	var mu sync.Mutex
	var got []wire.Series
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var p wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&p)
		mu.Lock()
		got = append(got, p.Series...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer intake.Close()

	at := time.Unix(1790000000, 0)
	ev := func(name string, t time.Time) dockerapi.Event {
		a := map[string]string{"name": name, "image": "sandbox:1", "exitCode": "3"}
		return dockerapi.Event{Type: "container", Action: "die", Actor: dockerapi.Actor{ID: strings.Repeat(name[:1], 64), Attributes: a}, Time: t.Unix(), TimeNano: t.UnixNano()}
	}
	api := &eventsAPI{
		evs:       []dockerapi.Event{ev("admiring_allen", at), ev("bold_bohr", at)},
		delivered: make(chan struct{}),
	}
	clk := testutil.NewFakeClock(at.Add(-time.Second))
	cfg := testConfig()
	cfg.Hostname = "box"
	cfg.Tags = []string{"env:test"}
	cfg.Intake.URL = intake.URL
	cfg.Statsd.Enabled = false
	cfg.Collectors.Docker.Enabled = true
	cfg.Collectors.Docker.MaxContainerNames = 1
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: clk, DockerAPI: api})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // onto the boundary, a second after the agent started: its first whole bucket (aggregator.Options.Started)
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()

	select {
	case <-api.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("event stream never read")
	}
	// The aggregator's ticker and the docker collector's start-up timer.
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 2 }, "not armed")
	clk.Advance(10 * time.Second)
	testutil.Eventually(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, s := range got {
			if s.Metric == "container.exits" {
				n++
			}
		}
		return n == 2
	}, "both exits not forwarded")
	mu.Lock()
	var names []string
	for _, s := range got {
		if s.Metric != "container.exits" {
			continue
		}
		for _, tg := range s.Tags {
			if strings.HasPrefix(tg, "container_name:") {
				names = append(names, tg)
			}
		}
	}
	mu.Unlock()
	slices.Sort(names)
	if want := []string{"container_name:admiring_allen", "container_name:other"}; !slices.Equal(names, want) {
		t.Fatalf("container_name tags = %v, want %v", names, want)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The shipped checks are in the default registry: configured instances
// become collectors, and a misconfigured one fails startup.
func TestNew_ShippedChecks(t *testing.T) {
	cfg := testConfig()
	cfg.Collectors.Checks = map[string]config.Check{
		"http_check": {Instances: []map[string]any{{"name": "home", "url": "http://127.0.0.1:1/"}}},
		"process":    {Instances: []map[string]any{{"process_name": "postgres"}}},
	}
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range a.sched.Collectors() {
		names = append(names, c.Name())
	}
	if !slices.Contains(names, "http_check:home") || !slices.Contains(names, "process") {
		t.Fatalf("collectors %v", names)
	}
	cfg.Collectors.Checks["http_check"] = config.Check{Instances: []map[string]any{{"url": "not a url"}}}
	if _, err := newAgent(t, cfg, Options{Logger: quiet}); err == nil || !strings.Contains(err.Error(), "collectors.checks") {
		t.Fatalf("err = %v", err)
	}
}

// labelledAPI is a daemon with one running container that asks for a check.
type labelledAPI struct{ eventsAPI }

func (*labelledAPI) ListContainers(context.Context) ([]dockerapi.Container, error) {
	return []dockerapi.Container{{
		ID: strings.Repeat("cd", 32), Names: []string{"/shop-cache-1"}, Image: "redis:7",
		Labels: map[string]string{"ozy.check.stub.port": "%%port%%"},
		Ports:  []dockerapi.Port{{PrivatePort: 6379}},
	}}, nil
}

// Autodiscovery end to end: a container's labels start a check whose
// output reaches the intake tagged like the container.
func TestRun_AutodiscoveryToIntake(t *testing.T) {
	testutil.CheckGoroutines(t)
	var mu sync.Mutex
	var got []wire.Series
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var p wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&p)
		mu.Lock()
		got = append(got, p.Series...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer intake.Close()

	var port int
	checks := collector.Registry{"stub": func(inst collector.Instance) (collector.Collector, error) {
		var cfg struct {
			Port int `yaml:"port"`
		}
		if err := inst.Decode(&cfg); err != nil {
			return nil, err
		}
		port = cfg.Port
		return stubCollector{}, nil
	}}
	clk := testutil.NewFakeClock(time.Unix(1790000001, 0))
	cfg := testConfig()
	cfg.Hostname = "box"
	cfg.Intake.URL = intake.URL
	cfg.Statsd.Enabled = false
	cfg.Collectors.Docker.Enabled = true
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: clk, DockerAPI: &labelledAPI{eventsAPI{delivered: make(chan struct{})}}, Checks: checks})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()

	testutil.Eventually(t, 2*time.Second, func() bool {
		for _, c := range a.sched.Collectors() {
			if c.Name() == "stub:shop-cache-1" {
				return true
			}
		}
		return false
	}, "the container's check was not started")
	if port != 6379 {
		t.Errorf("port = %d, want %%%%port%%%% resolved to 6379", port)
	}
	// Every timer armed (aggregator, docker, the stub's jitter, discovery),
	// then past the stub's 1s jitter.
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 4 }, "not armed")
	clk.Advance(time.Second)
	testutil.Eventually(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(got, func(s wire.Series) bool { return s.Metric == "stub.level" })
	}, "the discovered check's output was not forwarded")
	mu.Lock()
	i := slices.IndexFunc(got, func(s wire.Series) bool { return s.Metric == "stub.level" })
	tags := got[i].Tags
	mu.Unlock()
	for _, want := range []string{"container_name:shop-cache-1", "image_name:redis", "host:box", "k:v"} {
		if !slices.Contains(tags, want) {
			t.Errorf("tags %v lack %s", tags, want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
