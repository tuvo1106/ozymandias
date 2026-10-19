package monitor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/monitor/state"
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
// monitor to the API — unless some file's uid could not be read this pass, in
// which case nothing is released and the log says why.
func TestProvisionReleasesRemovedFiles(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	def := func(uid string) string {
		return `{"uid":"` + uid + `","name":"` + strings.ToUpper(uid) + `","type":"metric","query":"avg(last_5m):avg:x{*} > 1"}`
	}
	dir := testutil.TempDirWith(t, map[string]string{"a.json": def("a"), "b.json": def("b"), "c.json": def("c")})
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	pass := func(paths ...string) ProvisionResult {
		logs.Reset()
		return Provision(context.Background(), provisionDB{db}, paths, nil, t0, slog.New(slog.NewTextHandler(&logs, nil)))
	}
	if res := pass(dir); res.Loaded != 3 {
		t.Fatalf("%+v", res)
	}
	if err := os.Remove(filepath.Join(dir, "a.json")); err != nil {
		t.Fatal(err)
	}
	// A uid that cannot be read, or a directory that is missing: a's file may
	// be there, so nothing is released, and the log says which and why.
	write("b.json", `{"uid":`)
	if res := pass(dir); len(res.Released) != 0 || !strings.Contains(logs.String(), "not released") ||
		!strings.Contains(logs.String(), "b.json") {
		t.Fatalf("undecodable: %v\n%s", res.Released, logs.String())
	}
	write("b.json", def("b"))
	if os.Geteuid() != 0 { // root reads a 0o000 file anyway
		if err := os.Chmod(filepath.Join(dir, "b.json"), 0); err != nil {
			t.Fatal(err)
		}
		if res := pass(dir); len(res.Released) != 0 || !strings.Contains(logs.String(), "not released") {
			t.Fatalf("unreadable: %v\n%s", res.Released, logs.String())
		}
		if err := os.Chmod(filepath.Join(dir, "b.json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A dangling symlink and a directory named *.json are not files, so
	// they hold nothing back.
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "dir.json")); err != nil {
		t.Fatal(err)
	}
	if res := pass(dir); !slices.Equal(res.Released, []string{"A"}) {
		t.Fatalf("not-a-file held back the release: %v\n%s", res.Released, logs.String())
	}
	if err := db.DeleteMonitor(context.Background(), mustUID(t, db, "a")); err != nil {
		t.Fatal(err)
	}
	if res := pass(dir); len(res.Released) != 0 {
		t.Fatalf("re-released: %v", res.Released)
	}
	_ = os.Remove(filepath.Join(dir, "dangling.json"))
	_ = os.Remove(filepath.Join(dir, "dir.json"))
	_ = os.Remove(filepath.Join(dir, "sub"))
	// Put a back as a provisioned monitor with no file, for the cases below.
	if _, _, err := db.UpsertProvisionedMonitor(context.Background(), meta.MonitorRow{UID: "a", Name: "A", Type: "metric",
		Query: "avg(last_5m):avg:x{*} > 1", Enabled: true}, t0); err != nil {
		t.Fatal(err)
	}
	if res := pass(dir, filepath.Join(dir, "unmounted")); len(res.Released) != 0 || !strings.Contains(logs.String(), "unmounted") {
		t.Fatalf("missing dir: %v\n%s", res.Released, logs.String())
	}
	// A file whose uid is readable but which fails (unknown field, bad query)
	// is present, so it blocks nothing and is not itself released.
	write("b.json", `{"uid":"b","name":"B","type":"metric","query":"x","optoins":{}}`)
	if res := pass(dir); !slices.Equal(res.Released, []string{"A"}) {
		t.Fatalf("released %v\n%s", res.Released, logs.String())
	}
	if b, _ := db.MonitorByUID(context.Background(), "b"); !b.Provisioned {
		t.Fatal("a broken file's monitor was released")
	}
	// a's file comes back and is refused (its uid is now the API's) on every
	// pass; that failure does not stop c being released when c's file goes.
	write("a.json", def("a"))
	write("b.json", def("b"))
	if err := os.Remove(filepath.Join(dir, "c.json")); err != nil {
		t.Fatal(err)
	}
	if res := pass(dir); res.Failed != 1 || !slices.Equal(res.Released, []string{"C"}) {
		t.Fatalf("%+v\n%s", res, logs.String())
	}
}

func mustUID(t *testing.T, db *meta.DB, uid string) int64 {
	t.Helper()
	m, err := db.MonitorByUID(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// TestFileVanishingMidPassHoldsBackReleases: a plain file listed and then
// missing (a checkout replacing it) is an unknown uid, not a removal.
func TestFileVanishingMidPassHoldsBackReleases(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dir := t.TempDir()
	if _, _, err := db.UpsertProvisionedMonitor(context.Background(), meta.MonitorRow{UID: "x", Name: "X", Type: "metric",
		Query: "avg(last_5m):avg:x{*} > 1", Enabled: true}, t0); err != nil {
		t.Fatal(err)
	}
	var seen []string
	var res ProvisionResult
	err = provisionFile(context.Background(), provisionDB{db}, filepath.Join(dir, "x.json"), nil, t0, slog.New(slog.DiscardHandler), &res, &seen)
	if !errors.Is(err, errNoUID) {
		t.Fatalf("a listed file gone at read: %v", err)
	}
}

// TestFailedRenderKeepsMentions: a message that cannot render says so and
// still names everyone it was for, so the failure is seen.
func TestFailedRenderKeepsMentions(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	m.Message = "{{.Group.q}} @pager @log"
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	huge := spec.Group{"q": strings.Repeat("x", spec.MaxRenderedBytes+1)}
	n := h.s.render(c, meta.MonitorRow{ID: 1, Name: "m"}, huge, "q:x", state.Alert, state.OK, 0, t0, t0, t0)
	if !strings.HasPrefix(n.Message, "(the message template failed") || !strings.HasSuffix(n.Message, " @pager @log") {
		t.Fatalf("message %q", n.Message)
	}
}
