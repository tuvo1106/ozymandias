package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// TestQueryChangeAcrossRestartResets: a query edited before a restart (or
// while disabled) leaves no old groups behind to go NO_DATA, because the
// scheduler that loads afterwards never saw the old query.
func TestQueryChangeAcrossRestartResets(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	m.Options.NoDataTimeframeM = 1
	m.Message = "@cap"
	id := h.create(m)
	h.points("queue.depth", []string{"q:a"}, 0, 10*time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	row, _ := h.db.Monitor(context.Background(), id)
	row.Query = "last(last_1m):avg:other{*} by {q} > 20"
	if _, err := h.db.UpdateMonitor(context.Background(), id, row, t0); err != nil {
		t.Fatal(err)
	}
	h.s = h.scheduler() // restart before any evaluation sees the change
	for off := time.Minute; off <= 5*time.Minute; off += 30 * time.Second {
		h.evalAt(t0.Add(off))
	}
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("notifications %d, want only the original ALERT: %+v", n, h.out.notes(t))
	}
}

// TestDeletedDuringEvaluationWritesNothing: an evaluation holding a stale
// entry for a monitor deleted under it commits nothing and notifies no one.
func TestDeletedDuringEvaluationWritesNothing(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Message = "@cap"
	id := h.create(m)
	h.points("queue.depth", nil, 0, 40*time.Second, func(time.Duration) float64 { return 25 })
	h.points("queue.depth", nil, 40*time.Second, 5*time.Minute, func(time.Duration) float64 { return 0 })
	h.evalAt(t0.Add(30 * time.Second))
	e := h.s.entries[id]
	if err := h.db.DeleteMonitor(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(90 * time.Second)
	h.clk.Set(at)
	h.s.evaluate(context.Background(), e, e.compiled, e.row, at)
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("notified for a deleted monitor: %d", n)
	}
	if st, _ := h.db.States(context.Background(), id); len(st) != 0 {
		t.Fatalf("orphan states %v", st)
	}
	if h.s.commitErrors.Value() != 0 {
		t.Error("a deleted monitor counted as a commit error")
	}
}

// TestQueryChangedDuringEvaluationWritesNothing: an evaluation of the old
// query that finishes after the edit does not bring the old groups back.
func TestQueryChangedDuringEvaluationWritesNothing(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Message = "@cap"
	id := h.create(m)
	h.points("queue.depth", nil, 0, 5*time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	e := h.s.entries[id]
	row, _ := h.db.Monitor(context.Background(), id)
	row.Query = "last(last_1m):avg:other{*} > 20"
	if _, err := h.db.UpdateMonitor(context.Background(), id, row, t0); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(time.Minute)
	h.clk.Set(at)
	h.s.evaluate(context.Background(), e, e.compiled, e.row, at) // the old query, in flight
	if st, _ := h.db.States(context.Background(), id); len(st) != 0 {
		t.Fatalf("old query's groups came back: %v", st)
	}
}

// TestEvalErrorForDeletedMonitorIsNotRecorded: a failing evaluation whose
// monitor was deleted under it leaves no orphan event.
func TestEvalErrorForDeletedMonitorIsNotRecorded(t *testing.T) {
	h := newHarness(t)
	h.ev = &erroring{err: errors.New("timeout")}
	h.s = h.scheduler()
	id := h.create(backlog())
	h.evalAt(t0)
	e := h.s.entries[id]
	e.lastErr = ""
	if err := h.db.DeleteMonitor(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.s.evaluate(context.Background(), e, e.compiled, e.row, t0.Add(30*time.Second))
	if evs, _ := h.db.Events(context.Background(), meta.EventQuery{}); len(evs) != 0 {
		t.Fatalf("orphan events %+v", evs)
	}
}

// hostile returns one group whose tag value carries a mention.
type hostile struct{}

func (hostile) Evaluate(_ context.Context, _ *spec.Compiled, now time.Time) ([]eval.Value, eval.Data, error) {
	g := spec.Group{"q": "x @evil"}
	return []eval.Value{{Key: g.String(), Group: g, Present: true, Value: 25, DataTs: now}}, eval.Data{}, nil
}
func (hostile) Fetch(context.Context, *spec.Compiled, time.Time, time.Time) (eval.Data, error) {
	return eval.Data{}, nil
}

// TestDataCannotAddNotifiers: a tag value is data, and data does not choose
// who gets paged.
func TestDataCannotAddNotifiers(t *testing.T) {
	h := newHarness(t)
	h.ev = hostile{}
	h.s = h.scheduler()
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	m.Message = "{{.Group}} is high @cap"
	h.create(m)
	h.evalAt(t0.Add(30 * time.Second))
	var got []string
	for _, r := range h.out.rows {
		got = append(got, r.Notifier)
	}
	if !slices.Equal(got, []string{"cap"}) {
		t.Fatalf("routed to %v", got)
	}
}

// failingMonitors is a store whose monitor list cannot be read.
type failingMonitors struct{ *meta.DB }

func (failingMonitors) Monitors(context.Context) ([]meta.MonitorRow, error) {
	return nil, errors.New("database is locked")
}

// TestHealthWhileLoadingFails: a scheduler that cannot load its monitors is
// not "idle with nothing to do"; past the stall threshold it says stalled.
func TestHealthWhileLoadingFails(t *testing.T) {
	testutil.CheckGoroutines(t)
	h := newHarness(t)
	s := New(Options{Store: failingMonitors{h.db}, Evaluator: &synthetic{}, Clock: h.clk})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	testutil.Eventually(t, 5*time.Second, func() bool { return s.Health().Loading }, "loading")
	if s.Health().Stalled {
		t.Fatal("stalled at once")
	}
	for range 13 {
		testutil.Eventually(t, 5*time.Second, func() bool { return h.clk.Waiters() >= 1 }, "retry armed")
		h.clk.Advance(10 * time.Second)
	}
	testutil.Eventually(t, 5*time.Second, func() bool { return s.Health().Stalled }, "stalled while loading fails")
}

// TestUncompilableMonitorIsNotAStall: a stored definition that no longer
// compiles is never evaluated, so it cannot make the scheduler look stalled.
func TestUncompilableMonitorIsNotAStall(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.CreateMonitor(context.Background(), meta.MonitorRow{Name: "old", Type: "metric",
		Query: "from a newer build", Enabled: true}, t0); err != nil {
		t.Fatal(err)
	}
	h.evalAt(t0)
	if len(h.s.entries) != 1 {
		t.Fatalf("entries %d", len(h.s.entries))
	}
	h.clk.Set(t0.Add(time.Hour))
	if hl := h.s.Health(); hl.Monitors != 0 || hl.Stalled {
		t.Fatalf("health %+v", hl)
	}
	// The self-metric agrees with the API about what "monitors" means.
	for _, p := range h.s.opt.Registry.Snapshot() {
		if p.Name == "ozy.monitor.monitors" && p.Value != 0 {
			t.Errorf("ozy.monitor.monitors = %v counts a monitor that never runs", p.Value)
		}
	}
}

// TestProvisionReleasesRemovedFiles: deleting a provisioned file hands the
// monitor to the API rather than leaving it running and uneditable.
func TestProvisionReleasesRemovedFiles(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dir := testutil.TempDirWith(t, map[string]string{
		"a.json": `{"uid":"a","name":"A","type":"metric","query":"avg(last_5m):avg:x{*} > 1"}`,
		"b.json": `{"uid":"b","name":"B","type":"metric","query":"avg(last_5m):avg:x{*} > 1"}`,
	})
	if res := Provision(context.Background(), provisionDB{db}, []string{dir}, nil, t0, nil); res.Loaded != 2 {
		t.Fatalf("%+v", res)
	}
	good := `{"uid":"b","name":"B","type":"metric","query":"avg(last_5m):avg:x{*} > 1"}`
	if err := os.Remove(filepath.Join(dir, "a.json")); err != nil {
		t.Fatal(err)
	}
	// A pass that is not clean releases nothing: a file that does not even
	// decode (whose uid is unknowable), one that does not compile, or a
	// configured directory that is missing.
	for what, step := range map[string]struct {
		b     string
		paths []string
	}{
		"undecodable":  {`{"uid":"b",`, []string{dir}},
		"uncompilable": {`{"uid":"b","name":"B","type":"metric","query":"x"}`, []string{dir}},
		"missing dir":  {good, []string{dir, filepath.Join(dir, "unmounted")}},
	} {
		if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(step.b), 0o644); err != nil {
			t.Fatal(err)
		}
		if res := Provision(context.Background(), provisionDB{db}, step.paths, nil, t0, nil); len(res.Released) != 0 {
			t.Fatalf("%s: released %v", what, res.Released)
		}
	}
	for _, uid := range []string{"a", "b"} {
		if m, _ := db.MonitorByUID(context.Background(), uid); !m.Provisioned {
			t.Fatalf("%s released by an unclean pass", uid)
		}
	}
	// Clean: a is released, b is not.
	if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Provision(context.Background(), provisionDB{db}, []string{dir}, nil, t0, nil)
	if !slices.Equal(res.Released, []string{"A"}) {
		t.Fatalf("released %v", res.Released)
	}
	a, _ := db.MonitorByUID(context.Background(), "a")
	b, _ := db.MonitorByUID(context.Background(), "b")
	if a.Provisioned || !b.Provisioned {
		t.Fatalf("a %v b %v", a.Provisioned, b.Provisioned)
	}
}
