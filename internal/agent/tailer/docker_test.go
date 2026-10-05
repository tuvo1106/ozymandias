package tailer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// fakeDocker is a daemon: a list of containers and, for each, a log that
// can be opened (replaying from `since`) and followed.
type fakeDocker struct {
	mu         sync.Mutex
	containers []dockerapi.Container
	tty        map[string]bool
	logs       map[string]*fakeLog
	opens      []openCall
	failOpen   error
}

type openCall struct {
	id    string
	since time.Time
}

type fakeLog struct {
	lines []fakeLine
	// live, when set, is the open stream's pipe writer.
	pw   *io.PipeWriter
	done bool // close the stream after replaying: the container stopped
}

type fakeLine struct {
	ts     time.Time
	text   string
	stderr bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{tty: map[string]bool{}, logs: map[string]*fakeLog{}}
}

func (f *fakeDocker) add(id, name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = append(f.containers, dockerapi.Container{ID: id, Names: []string{"/" + name}, Labels: labels})
	f.logs[id] = &fakeLog{}
}

func (f *fakeDocker) ListContainers(context.Context) ([]dockerapi.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockerapi.Container(nil), f.containers...), nil
}

func (f *fakeDocker) Inspect(_ context.Context, id string) (dockerapi.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var j dockerapi.ContainerJSON
	j.Config.Tty = f.tty[id]
	return j, nil
}

func encode(l fakeLine, tty bool) []byte {
	text := l.ts.UTC().Format(time.RFC3339Nano) + " " + l.text + "\n"
	if tty {
		return []byte(text)
	}
	k := byte(1)
	if l.stderr {
		k = 2
	}
	return frame(k, text)
}

func (f *fakeDocker) Logs(ctx context.Context, id string, since time.Time, follow bool) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, openCall{id, since})
	if f.failOpen != nil {
		return nil, f.failOpen
	}
	lg := f.logs[id]
	if lg == nil {
		return nil, dockerapi.ErrNotFound
	}
	var replay bytes.Buffer
	for _, l := range lg.lines {
		if since.IsZero() || !l.ts.Before(since) { // the daemon replays from `since` inclusive
			replay.Write(encode(l, f.tty[id]))
		}
	}
	if lg.done {
		return io.NopCloser(&replay), nil
	}
	pr, pw := io.Pipe()
	lg.pw = pw
	go func() {
		_, _ = pw.Write(replay.Bytes())
	}()
	go func() { <-ctx.Done(); _ = pw.Close() }()
	return pr, nil
}

// emit appends a line to a container's log and, if a stream is open, writes it.
func (f *fakeDocker) emit(id string, l fakeLine) {
	f.mu.Lock()
	lg := f.logs[id]
	lg.lines = append(lg.lines, l)
	pw, tty := lg.pw, f.tty[id]
	f.mu.Unlock()
	if pw != nil {
		_, _ = pw.Write(encode(l, tty))
	}
}

// breakStream cuts the open stream the way a daemon restart does.
func (f *fakeDocker) breakStream(id string) {
	f.mu.Lock()
	pw := f.logs[id].pw
	f.logs[id].pw = nil
	f.mu.Unlock()
	if pw != nil {
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
	}
}

func (f *fakeDocker) opensOf(id string) []openCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []openCall
	for _, o := range f.opens {
		if o.id == id {
			out = append(out, o)
		}
	}
	return out
}

type dockerRig struct {
	t    *testing.T
	api  *fakeDocker
	clk  *testutil.FakeClock
	sink *recSink
	reg  *Registry
	d    *Docker
	stop context.CancelFunc
}

func newDockerRig(t *testing.T, src DockerSource, tune ...func(*DockerOptions)) *dockerRig {
	t.Helper()
	r := &dockerRig{t: t, api: newFakeDocker(), clk: testutil.NewFakeClock(t0), sink: &recSink{}}
	r.reg, _ = OpenRegistry("")
	if src.Pipeline.RateLimit == 0 {
		src.Pipeline.RateLimit = -1
	}
	r.build([]DockerSource{src}, tune...)
	return r
}

func (r *dockerRig) build(src []DockerSource, tune ...func(*DockerOptions)) {
	opts := DockerOptions{API: r.api, Registry: r.reg, Sink: r.sink, Host: "box", Clock: r.clk, BackoffMin: time.Second}
	for _, f := range tune {
		f(&opts)
	}
	d, err := NewDocker(src, opts)
	if err != nil {
		r.t.Fatal(err)
	}
	r.d = d
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	r.d.mu.Lock()
	r.d.ctx = ctx
	r.d.mu.Unlock()
	r.t.Cleanup(func() { cancel(); r.d.wg.Wait() })
}

func (r *dockerRig) scan() { r.d.Scan(r.d.ctx) }

func (r *dockerRig) wantMessages(want ...string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := r.sink.messages()
		if len(got) == len(want) {
			eq(r.t, got, want...)
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("got %q\nwant %q", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

var composeLabels = map[string]string{labelComposeProj: "proj", labelComposeSvc: "api"}

func TestDocker_FollowsMatchingContainersWithTheirServiceAndSource(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{labelComposeProj + "=proj"}, Source: "python", Tags: []string{"env:dev"}, StartPosition: "beginning"})
	r.api.add("c1", "proj-api-1", composeLabels)
	r.api.add("c2", "other-1", map[string]string{labelComposeProj: "elsewhere"})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "INFO:     started", true}, {at(2), "plain line", false}}
	r.api.logs["c2"].lines = []fakeLine{{at(1), "not mine", false}}
	r.scan()
	r.wantMessages("started", "plain line")
	l := r.sink.logs[0]
	if l.Service != "proj-api" || l.Source != "python" || l.Host != "box" || l.Status != wire.StatusInfo || l.Tags[0] != "env:dev" {
		t.Fatalf("%+v", l)
	}
	// stderr only decides when nothing recognised the line.
	if r.sink.logs[1].Status != wire.StatusInfo {
		t.Fatalf("%+v", r.sink.logs[1])
	}
}

func TestDocker_UnrecognisedStderrIsAnError(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, Source: "python", StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "something odd", true}}
	r.scan()
	r.wantMessages("something odd")
	if r.sink.logs[0].Status != wire.StatusError {
		t.Fatalf("%+v", r.sink.logs[0])
	}
}

func TestDocker_ExcludedNamesAreNeverTailed(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, ExcludeNames: []string{`^judge-`}, StartPosition: "beginning"})
	r.api.add("c1", "judge-123", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "user program output", false}}
	r.scan()
	time.Sleep(20 * time.Millisecond)
	if len(r.api.opensOf("c1")) != 0 || len(r.sink.messages()) != 0 {
		t.Fatal("an excluded container was tailed")
	}
	if _, err := NewDocker([]DockerSource{{ExcludeNames: []string{"("}}}, DockerOptions{}); err == nil {
		t.Fatal("bad exclude pattern accepted")
	}
}

func TestDocker_LabelsConfigureAContainerWithNoAgentConfig(t *testing.T) {
	r := newDockerRig(t, DockerSource{}, func(o *DockerOptions) { o.CollectAll = false })
	r.api.add("c1", "mystery", map[string]string{
		LabelLogsEnabled: "true", LabelLogsSource: "json", LabelLogsService: "billing", LabelLogsTags: "team:pay, tier:1",
	})
	r.api.add("c2", "quiet", map[string]string{"x": "y"}) // no opt-in, not collect-all
	r.api.logs["c1"].lines = []fakeLine{{at(1), `{"level":"warn","message":"hi"}`, false}}
	r.api.logs["c2"].lines = []fakeLine{{at(1), "ignored", false}}
	// First scan is the agent's start: history is skipped. Make the log post-date it.
	r.api.logs["c1"].lines[0].ts = t0.Add(time.Hour)
	r.scan()
	r.wantMessages("hi")
	l := r.sink.logs[0]
	if l.Service != "billing" || l.Status != wire.StatusWarn || len(l.Tags) != 2 || l.Tags[0] != "team:pay" || l.Tags[1] != "tier:1" {
		t.Fatalf("%+v", l)
	}
	if len(r.api.opensOf("c2")) != 0 {
		t.Fatal("a container that did not opt in was tailed")
	}
}

func TestDocker_OptOutLabelWinsEvenOverCollectAll(t *testing.T) {
	r := newDockerRig(t, DockerSource{}, func(o *DockerOptions) { o.CollectAll = true })
	r.api.add("c1", "loud", map[string]string{LabelLogsEnabled: "false"})
	r.api.add("c2", "other", nil)
	r.scan()
	time.Sleep(20 * time.Millisecond)
	if len(r.api.opensOf("c1")) != 0 || len(r.api.opensOf("c2")) != 1 {
		t.Fatalf("c1 opens %d, c2 opens %d", len(r.api.opensOf("c1")), len(r.api.opensOf("c2")))
	}
	for _, v := range []string{"false", "0", "No", "OFF"} {
		if !isFalse(v) {
			t.Errorf("%q should disable", v)
		}
	}
	if isFalse("true") || isFalse("") {
		t.Error("true/empty must not disable")
	}
}

func TestDocker_AContainerFoundAtStartSkipsHistoryButALaterOneStartsAtZero(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}})
	r.api.add("old", "old", map[string]string{"a": ""})
	r.api.logs["old"].lines = []fakeLine{{t0.Add(-time.Hour), "history", false}}
	r.scan()
	r.api.emit("old", fakeLine{t0.Add(time.Second), "fresh", false})
	r.wantMessages("fresh")

	r.api.add("new", "new", map[string]string{"a": ""})
	r.api.logs["new"].lines = []fakeLine{{t0.Add(-time.Hour), "its first line", false}}
	r.scan()
	r.wantMessages("fresh", "its first line")
}

func TestDocker_ReconnectResumesFromTheLastAckAndDropsReplays(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "one", false}, {at(2), "two", false}}
	r.scan()
	r.wantMessages("one", "two")
	waitFor(t, func() bool { e, _ := r.reg.Get("docker:c1"); return e.TS == at(2).UnixNano() })

	r.api.breakStream("c1")
	waitFor(t, func() bool { return r.d.Stats().Reconnects == 1 })
	waitFor(t, func() bool { return r.clk.Waiters() > 0 }) // the backoff timer (the pump's ticker is stopped)
	r.api.emit("c1", fakeLine{at(3), "three", false})      // written while disconnected
	r.clk.Advance(time.Second)
	r.wantMessages("one", "two", "three")
	opens := r.api.opensOf("c1")
	if len(opens) != 2 || !opens[1].since.Equal(at(2)) {
		t.Fatalf("opens: %+v; the second must resume from the last acknowledged timestamp", opens)
	}
	if r.d.Stats().Reconnects != 1 {
		t.Fatalf("%+v", r.d.Stats())
	}
}

func TestDocker_TheTimestampIsCommittedOnlyAfterTheSinkAccepts(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "one", false}}
	r.sink.fail = errors.New("intake down")
	r.scan()
	waitFor(t, func() bool { return r.d.Stats().SendErrors > 0 })
	if e, _ := r.reg.Get("docker:c1"); e.TS != 0 {
		t.Fatalf("committed %d while the sink was failing", e.TS)
	}
	r.sink.mu.Lock()
	r.sink.fail = nil
	r.sink.mu.Unlock()
	waitFor(t, func() bool { return r.d.Stats().Reconnects >= 1 })
	waitFor(t, func() bool { return r.clk.Waiters() > 0 })
	r.clk.Advance(time.Second)
	r.wantMessages("one")
	waitFor(t, func() bool { e, _ := r.reg.Get("docker:c1"); return e.TS == at(1).UnixNano() })
}

func TestDocker_ARestartedAgentResumesFromTheRegistry(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "acked", false}, {at(2), "missed while down", false}}
	r.reg.Set("docker:c1", Entry{TS: at(1).UnixNano(), LastSeen: t0.Unix()})
	r.scan()
	r.wantMessages("missed while down")
}

func TestDocker_MultilineTracebacksAreOneEventAndFlushOnTheTimer(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning", MultilineStart: `^(INFO|ERROR)`, Source: "python"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{
		{at(1), "ERROR boom", true}, {at(2), "Traceback (most recent call last):", true}, {at(3), "ValueError: no", true},
	}
	r.scan()
	waitFor(t, func() bool { return r.clk.Waiters() > 0 })
	time.Sleep(20 * time.Millisecond)
	if len(r.sink.messages()) != 0 {
		t.Fatal("a pending multiline event was emitted before its flush timeout")
	}
	r.clk.Advance(MultilineFlushAfter + 300*time.Millisecond)
	r.wantMessages("ERROR boom\nTraceback (most recent call last):\nValueError: no")
}

func TestDocker_TTYContainersAreReadRaw(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.tty["c1"] = true
	r.api.logs["c1"].lines = []fakeLine{{at(1), "from a terminal", false}}
	r.scan()
	r.wantMessages("from a terminal")
}

func TestDocker_AStoppedContainerIsDrainedThenForgottenAndFollowedAgainWhenItReturns(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "before stop", false}}
	r.api.logs["c1"].done = true // the stream ends after replaying
	r.scan()
	r.wantMessages("before stop")
	waitFor(t, func() bool { r.d.mu.Lock(); defer r.d.mu.Unlock(); c := r.d.conts["c1"]; return c != nil && isDone(c) })
	// Restarted: listed again, new line.
	r.api.mu.Lock()
	r.api.logs["c1"].lines = append(r.api.logs["c1"].lines, fakeLine{at(5), "after restart", false})
	r.api.mu.Unlock()
	r.scan()
	r.wantMessages("before stop", "after restart")
	if o := r.api.opensOf("c1"); len(o) != 2 || !o[1].since.Equal(at(1)) {
		t.Fatalf("%+v", o)
	}
}

func isDone(c *cont) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func TestDocker_ADeletedContainerStopsTheFollower(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.failOpen = fmt.Errorf("opening: %w", dockerapi.ErrNotFound)
	r.scan()
	waitFor(t, func() bool { r.d.mu.Lock(); defer r.d.mu.Unlock(); c := r.d.conts["c1"]; return c != nil && isDone(c) })
	if r.d.Stats().Reconnects != 0 {
		t.Fatal("a gone container must not be retried")
	}
}

func TestDocker_RunScansOnTheTickerAndStopsCleanly(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.logs["c1"].lines = []fakeLine{{at(1), "hello", false}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.d.Run(ctx); close(done) }()
	r.wantMessages("hello")
	cancel()
	<-done
}

func TestDocker_ListErrorsAreSurvived(t *testing.T) {
	r := newDockerRig(t, DockerSource{})
	r.d.opts.API = errList{}
	r.scan() // must not panic or start anything
	if r.d.Stats().Containers != 0 {
		t.Fatal("started followers from a failed list")
	}
}

type errList struct{ DockerAPI }

func (errList) ListContainers(context.Context) ([]dockerapi.Container, error) {
	return nil, errors.New("daemon down")
}

func TestDocker_AFailingOpenBacksOffExponentially(t *testing.T) {
	r := newDockerRig(t, DockerSource{IncludeLabels: []string{"a"}, StartPosition: "beginning"})
	r.api.add("c1", "x", map[string]string{"a": ""})
	r.api.failOpen = errors.New("daemon hiccup")
	r.scan()
	step := func(adv time.Duration, wantOpens int) {
		t.Helper()
		waitFor(t, func() bool { return r.clk.Waiters() > 0 })
		r.clk.Advance(adv)
		waitFor(t, func() bool { return len(r.api.opensOf("c1")) >= wantOpens })
		time.Sleep(10 * time.Millisecond)
		if n := len(r.api.opensOf("c1")); n != wantOpens {
			t.Fatalf("%d opens after advancing %v, want %d", n, adv, wantOpens)
		}
	}
	waitFor(t, func() bool { return len(r.api.opensOf("c1")) == 1 })
	step(time.Second, 2) // first backoff: 1s
	step(time.Second, 2) // the second is 2s: one second is not enough
	step(time.Second, 3) // ... now it is
}

func TestDocker_ServiceNameFallsBackFromLabelToComposeToContainerName(t *testing.T) {
	d, _ := NewDocker([]DockerSource{{IncludeLabels: []string{"a"}}}, DockerOptions{})
	for _, c := range []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"a": "", labelService: "named", labelComposeProj: "p", labelComposeSvc: "s"}, "named"},
		{map[string]string{"a": "", labelComposeProj: "p", labelComposeSvc: "s"}, "p-s"},
		{map[string]string{"a": ""}, "ctr"},
	} {
		cc, ok := d.configFor(dockerapi.Container{ID: "x", Names: []string{"/ctr"}, Labels: c.labels})
		if !ok || cc.service != c.want {
			t.Errorf("%v: service %q, want %q", c.labels, cc.service, c.want)
		}
	}
}
