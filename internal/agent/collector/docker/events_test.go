package docker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func event(action, id string, at time.Time, attrs map[string]string) dockerapi.Event {
	a := map[string]string{"name": "job-1", "image": "worker:2"}
	for k, v := range attrs {
		a[k] = v
	}
	return dockerapi.Event{Type: "container", Action: action, Actor: dockerapi.Actor{ID: id, Attributes: a}, Time: at.Unix(), TimeNano: at.UnixNano()}
}

type samples struct {
	mu  sync.Mutex
	got []Sample
}

func (s *samples) add(x Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, x)
}

func (s *samples) all() []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *samples) named(name string) []Sample {
	var out []Sample
	for _, x := range s.all() {
		if x.Name == name {
			out = append(out, x)
		}
	}
	return out
}

// watch runs a watcher over api until the test ends.
func watch(t *testing.T, api *fakeAPI, opts WatcherOptions) (*samples, *testutil.FakeClock, *selfmetrics.Registry) {
	t.Helper()
	sk := &samples{}
	fc := testutil.NewFakeClock(t0)
	reg := selfmetrics.NewRegistry()
	opts.API, opts.Sink, opts.Clock, opts.Registry = api, sk.add, fc, reg
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	w := NewWatcher(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop")
		}
	})
	return sk, fc, reg
}

func TestWatcher_ExitAndLifetime(t *testing.T) {
	id := "e" + idAPI[1:]
	api := &fakeAPI{
		scripts: [][]dockerapi.Event{{
			event("start", id, t0, nil),
			event("die", id, t0.Add(2500*time.Millisecond), map[string]string{"exitCode": "3", LabelComposeProject: "shop"}),
		}},
		errs: []error{nil},
	}
	sk, _, _ := watch(t, api, WatcherOptions{})
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 2 }, "samples")
	ex := sk.named("container.exits")[0]
	if ex.Kind != CountSample || ex.Value != 1 {
		t.Errorf("exits = %+v", ex)
	}
	for _, want := range []string{"exit_code:3", "oom_killed:false", "container_name:job-1", "image_name:worker", "compose_project:shop"} {
		if !slices.Contains(ex.Tags, want) {
			t.Errorf("exits tags %v lack %s", ex.Tags, want)
		}
	}
	lt := sk.named("container.lifetime")[0]
	if lt.Kind != DistributionSample || lt.Value != 2.5 {
		t.Errorf("lifetime = %+v, want 2.5s", lt)
	}
}

func TestWatcher_OOMKill(t *testing.T) {
	id := "f" + idAPI[1:]
	api := &fakeAPI{
		scripts: [][]dockerapi.Event{{
			event("start", id, t0, nil),
			event("oom", id, t0.Add(time.Second), nil),
			event("die", id, t0.Add(time.Second), map[string]string{"exitCode": "137"}),
		}},
		errs: []error{nil},
	}
	sk, _, _ := watch(t, api, WatcherOptions{})
	testutil.Eventually(t, time.Second, func() bool { return len(sk.named("container.exits")) == 1 }, "exit")
	if tags := sk.named("container.exits")[0].Tags; !slices.Contains(tags, "oom_killed:true") || !slices.Contains(tags, "exit_code:137") {
		t.Errorf("tags = %v", tags)
	}
}

// A die whose start the watcher never saw (it connected later): the start
// comes from inspect if the container is still there, else no lifetime.
func TestWatcher_DieWithoutStart(t *testing.T) {
	kept, gone := "a"+idAPI[1:], "b"+idAPI[1:]
	api := &fakeAPI{
		inspect: map[string]dockerapi.ContainerJSON{kept: {State: dockerapi.ContainerState{StartedAt: t0.Add(-time.Minute), OOMKilled: true}}},
		scripts: [][]dockerapi.Event{{event("die", kept, t0, map[string]string{"exitCode": "137"}), event("die", gone, t0, nil)}},
		errs:    []error{nil},
	}
	sk, _, _ := watch(t, api, WatcherOptions{})
	testutil.Eventually(t, time.Second, func() bool { return len(sk.named("container.exits")) == 2 }, "exits")
	lts := sk.named("container.lifetime")
	if len(lts) != 1 || lts[0].Value != 60 {
		t.Fatalf("lifetimes = %+v, want one of 60s", lts)
	}
	ex := sk.named("container.exits")
	if !slices.Contains(ex[0].Tags, "oom_killed:true") {
		t.Errorf("inspect's OOM flag lost: %v", ex[0].Tags)
	}
	if !slices.Contains(ex[1].Tags, "exit_code:unknown") {
		t.Errorf("a die without an exit code: %v", ex[1].Tags)
	}
}

// The stream drops; the watcher reconnects asking for events since the last
// one it saw, and does not count that one twice.
func TestWatcher_ResumesWithoutDoubleCounting(t *testing.T) {
	a, b := "a"+idAPI[1:], "b"+idAPI[1:]
	dieA := event("die", a, t0.Add(time.Second), map[string]string{"exitCode": "0"})
	api := &fakeAPI{
		scripts: [][]dockerapi.Event{
			{event("start", a, t0, nil), dieA},
			{dieA, event("die", b, t0.Add(2*time.Second), map[string]string{"exitCode": "1"})}, // since is inclusive: dieA again
		},
		errs: []error{dockerapi.ErrStreamClosed, nil},
	}
	sk, fc, reg := watch(t, api, WatcherOptions{})
	testutil.Eventually(t, time.Second, func() bool { return len(sk.named("container.exits")) == 1 }, "first exit")
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "backoff timer")
	fc.Advance(minBackoff)
	testutil.Eventually(t, time.Second, func() bool { return len(sk.named("container.exits")) == 2 }, "second exit")
	time.Sleep(20 * time.Millisecond)
	if n := len(sk.named("container.exits")); n != 2 {
		t.Fatalf("%d exits, want 2: the replayed die was counted again", n)
	}
	api.mu.Lock()
	sinces := slices.Clone(api.sinces)
	api.mu.Unlock()
	if len(sinces) < 2 || !sinces[0].IsZero() || !sinces[1].Equal(dieA.At()) {
		t.Fatalf("since = %v, want zero then the last event's time", sinces)
	}
	if n := reg.Counter("ozy.agent.docker.events_reconnects").Value(); n < 1 {
		t.Errorf("reconnects = %d", n)
	}
}

// Docker not running: logged once, retried with growing backoff.
func TestWatcher_AnUnreachableDaemonIsLoggedOnce(t *testing.T) {
	down := errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")
	api := &fakeAPI{scripts: [][]dockerapi.Event{nil, nil, nil}, errs: []error{down, down, down}}
	var buf syncBuf
	_, fc, _ := watch(t, api, WatcherOptions{Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	for _, wait := range []time.Duration{minBackoff, 2 * minBackoff, 4 * minBackoff} {
		testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "backoff")
		fc.Advance(wait)
	}
	testutil.Eventually(t, time.Second, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.sinces) == 4
	}, "retries")
	if n := strings.Count(buf.String(), "docker event stream failed"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, buf.String())
	}
	// No event was ever seen, so every retry replays from the first
	// attempt: a container that died while the daemon was away is counted.
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.sinces[0].IsZero() || !api.sinces[1].Equal(t0) || !api.sinces[3].Equal(t0) {
		t.Fatalf("since = %v, want zero, then %v", api.sinces, t0)
	}
}

// An undecodable line is counted and the stream goes on.
func TestWatcher_CountsSkippedLines(t *testing.T) {
	api := &fakeAPI{
		scripts: [][]dockerapi.Event{{event("die", idAPI, t0, map[string]string{"exitCode": "0"})}},
		errs:    []error{nil},
		skips:   2,
	}
	sk, _, reg := watch(t, api, WatcherOptions{})
	testutil.Eventually(t, time.Second, func() bool { return len(sk.named("container.exits")) == 1 }, "exit")
	if n := reg.Counter("ozy.agent.docker.events_skipped").Value(); n != 2 {
		t.Fatalf("skipped = %d, want 2", n)
	}
}

func TestWatcher_OnStart(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	api := &fakeAPI{scripts: [][]dockerapi.Event{{event("start", idAPI, t0, nil)}}, errs: []error{nil}}
	watch(t, api, WatcherOptions{OnStart: func(id string) { mu.Lock(); ids = append(ids, id); mu.Unlock() }})
	testutil.Eventually(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(ids) == 1 && ids[0] == idAPI }, "OnStart not called")
}

func TestWatcher_Rewrites(t *testing.T) {
	id := "c" + idAPI[1:]
	api := &fakeAPI{
		scripts: [][]dockerapi.Event{{event("die", id, t0, map[string]string{"name": "judge-9-go", "exitCode": "0"})}},
		errs:    []error{nil},
	}
	sk, _, _ := watch(t, api, WatcherOptions{Rewrites: []Rewrite{{Match: regexp.MustCompile(`^judge-.*`), Replace: "judge"}}})
	testutil.Eventually(t, time.Second, func() bool { return len(sk.named("container.exits")) == 1 }, "exit")
	tags := sk.named("container.exits")[0].Tags
	if !slices.Contains(tags, "container_name:judge") || strings.Contains(strings.Join(tags, ","), "container_id") {
		t.Errorf("tags = %v", tags)
	}
}

func TestWatcher_BoundsWhatItTracks(t *testing.T) {
	w := NewWatcher(WatcherOptions{API: &fakeAPI{}, Sink: func(Sample) {}})
	for i := range maxTracked + 5 {
		_ = w.handle(context.Background(), event("start", strings.Repeat("0", 60)+string(rune('a'+i%26))+time.Duration(i).String(), t0.Add(time.Duration(i)), nil))
	}
	if len(w.started) > maxTracked {
		t.Fatalf("tracking %d starts", len(w.started))
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
