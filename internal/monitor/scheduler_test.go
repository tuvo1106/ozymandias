package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/notify"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
	qeval "github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// captured records what the scheduler handed to delivery.
type captured struct {
	mu   sync.Mutex
	rows []meta.OutboxRow
}

func (c *captured) Enqueue(_ context.Context, rows []meta.OutboxRow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, rows...)
}

// notes decodes everything enqueued so far.
func (c *captured) notes(t *testing.T) []notify.Notification {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []notify.Notification
	for _, r := range c.rows {
		var n notify.Notification
		if err := json.Unmarshal(r.Payload, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// flakyStore fails Commit while failing is set.
type flakyStore struct {
	*meta.DB
	mu      sync.Mutex
	failing bool
}

func (f *flakyStore) Commit(ctx context.Context, c meta.Commit) ([]meta.OutboxRow, error) {
	f.mu.Lock()
	failing := f.failing
	f.mu.Unlock()
	if failing {
		return nil, errors.New("database is locked")
	}
	return f.DB.Commit(ctx, c)
}

func (f *flakyStore) fail(on bool) {
	f.mu.Lock()
	f.failing = on
	f.mu.Unlock()
}

type harness struct {
	t     *testing.T
	db    *meta.DB
	store *flakyStore
	ms    *naive.Store
	clk   *testutil.FakeClock
	out   *captured
	s     *Scheduler
	ev    Evaluator
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ms, err := naive.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	h := &harness{t: t, db: db, store: &flakyStore{DB: db}, ms: ms, clk: testutil.NewFakeClock(t0), out: &captured{}}
	h.ev = &eval.Evaluator{Metrics: &qeval.Evaluator{Store: ms, Timeout: -1}}
	h.s = h.scheduler()
	return h
}

// scheduler builds a scheduler over the harness's database — a second call is
// a restart.
func (h *harness) scheduler() *Scheduler {
	return New(Options{Store: h.store, Evaluator: h.ev, Outbox: h.out, Clock: h.clk, BaseURL: "http://ozy"})
}

func (h *harness) create(m spec.Monitor) int64 {
	h.t.Helper()
	if _, err := spec.Compile(m); err != nil {
		h.t.Fatal(err)
	}
	opts, _ := json.Marshal(m.Options)
	row, err := h.db.CreateMonitor(context.Background(), meta.MonitorRow{UID: m.UID, Name: m.Name, Type: string(m.Type),
		Query: m.Query, Options: opts, Message: m.Message, Tags: m.Tags, Enabled: true}, t0)
	if err != nil {
		h.t.Fatal(err)
	}
	return row.ID
}

// points writes value(off) every 10s for off in [from, to); NaN is no point.
func (h *harness) points(metric string, tags []string, from, to time.Duration, value func(time.Duration) float64) {
	h.t.Helper()
	var smp []tsdb.Sample
	for off := from; off < to; off += 10 * time.Second {
		if v := value(off); !math.IsNaN(v) {
			smp = append(smp, tsdb.Sample{T: t0.Add(off).UnixMilli(), V: v})
		}
	}
	if _, err := h.ms.Append(context.Background(), []tsdb.SeriesSamples{{Series: tsdb.NewSeriesRef(metric, tags), Samples: smp}}); err != nil {
		h.t.Fatal(err)
	}
}

// evalAt runs every scheduled monitor once at t, synchronously.
func (h *harness) evalAt(t time.Time) {
	h.t.Helper()
	ctx := context.Background()
	h.clk.Set(t)
	if err := h.s.load(ctx, h.s.started.IsZero()); err != nil {
		h.t.Fatal(err)
	}
	h.s.started = t0
	h.s.mu.Lock()
	entries := make([]*entry, 0, len(h.s.entries))
	for _, e := range h.s.entries {
		entries = append(entries, e)
	}
	h.s.mu.Unlock()
	for _, e := range entries {
		h.s.evaluate(ctx, e, e.compiled, e.row, t)
	}
}

func (h *harness) transitions(id int64) []string {
	h.t.Helper()
	evs, err := h.db.Events(context.Background(), meta.EventQuery{MonitorID: id, Kinds: []string{EventTransition, EventRenotify}})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		out = append(out, e.Ts.UTC().Format("15:04:05")+" "+e.Kind+" "+e.From+">"+e.To)
	}
	return out
}

func eqStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n got: %q\nwant: %q", what, got, want)
	}
}

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

// backlog is the spec's example monitor, on a one-minute window with no delay
// so the scenario below reads cleanly.
func backlog() spec.Monitor {
	return spec.Monitor{UID: "backlog", Name: "Backlog", Type: spec.TypeMetric,
		Query: "last(last_1m):avg:queue.depth{*} > 20",
		Options: spec.Options{
			Thresholds:          spec.Thresholds{Critical: f(20), Warning: f(10), CriticalRecovery: f(15), WarningRecovery: f(8)},
			EvaluationIntervalS: 30, EvaluationDelayS: i(0),
			NotifyNoData: true, NoDataTimeframeM: 2,
		},
		Message: "{{.Monitor}} is {{.Status}} at {{.Value}}{{if .IsRecovery}} (recovered){{end}} @cap @pager",
	}
}

// scenarioTimeline: ok → warn → alert → hover in the hysteresis band →
// recover → silence → no-data → return.
func scenarioTimeline(h *harness) {
	hover := []float64{16, 21, 19, 24, 15.5}
	h.points("queue.depth", nil, 0, 18*time.Minute+time.Second, func(off time.Duration) float64 {
		switch {
		case off < 2*time.Minute:
			return 5
		case off < 4*time.Minute:
			return 12
		case off < 6*time.Minute:
			return 25
		case off < 10*time.Minute:
			return hover[int(off/(10*time.Second))%len(hover)]
		case off < 12*time.Minute:
			return 5
		case off < 16*time.Minute:
			return math.NaN() // silence
		default:
			return 5
		}
	})
}

// TestScenario is L8: a metric timeline through every state, asserting the
// exact event and notification sequence.
func TestScenario(t *testing.T) {
	h := newHarness(t)
	id := h.create(backlog())
	scenarioTimeline(h)
	for off := 30 * time.Second; off <= 18*time.Minute; off += 30 * time.Second {
		h.evalAt(t0.Add(off))
	}
	eqStrings(t, "transitions", h.transitions(id), []string{
		"12:02:30 transition OK>WARN",
		"12:04:30 transition WARN>ALERT",
		"12:10:30 transition ALERT>OK",
		"12:14:00 transition OK>NO_DATA",
		"12:16:30 transition NO_DATA>OK",
	})
	var got []string
	for _, n := range h.out.notes(t) {
		got = append(got, n.PrevStatus+">"+n.Status+" "+n.Message)
	}
	// One row per notifier the template names: every transition goes to
	// both (ADR-0052).
	eqStrings(t, "notifications", got, []string{
		"OK>WARN Backlog is WARN at 12 @cap @pager", "OK>WARN Backlog is WARN at 12 @cap @pager",
		"WARN>ALERT Backlog is ALERT at 25 @cap @pager", "WARN>ALERT Backlog is ALERT at 25 @cap @pager",
		"ALERT>OK Backlog is OK at 5 (recovered) @cap @pager", "ALERT>OK Backlog is OK at 5 (recovered) @cap @pager",
		"OK>NO_DATA Backlog is NO_DATA at NaN @cap @pager", "OK>NO_DATA Backlog is NO_DATA at NaN @cap @pager",
		"NO_DATA>OK Backlog is OK at 5 (recovered) @cap @pager", "NO_DATA>OK Backlog is OK at 5 (recovered) @cap @pager",
	})
	// A WARN reports the warning level it crossed, not the critical one.
	if ns := h.out.notes(t); ns[0].Threshold != 10 || ns[2].Threshold != 20 {
		t.Errorf("thresholds %g %g", ns[0].Threshold, ns[2].Threshold)
	}
	h.out.mu.Lock()
	var routed []string
	for _, r := range h.out.rows {
		routed = append(routed, r.Notifier)
	}
	h.out.mu.Unlock()
	eqStrings(t, "routing", routed, []string{"cap", "pager", "cap", "pager", "cap", "pager", "cap", "pager", "cap", "pager"})
	n := h.out.notes(t)[6]
	if n.Value != nil || !strings.HasPrefix(n.Link, "http://ozy/monitors/") {
		t.Errorf("no-data notification %+v", n)
	}
}

// TestNoDataWaitsForTheWindowToEmpty pins what docs/operations.md says about
// no-data timing: a group is absent only once its window holds no points, so a
// series that stops goes NO_DATA after the longer of the window and
// no_data_timeframe_m. Found live: a heartbeat monitor on a five-minute window
// with a three-minute timeframe went NO_DATA after five.
func TestNoDataWaitsForTheWindowToEmpty(t *testing.T) {
	h := newHarness(t)
	silent := func(window string, delay int) spec.Monitor {
		return spec.Monitor{UID: "hb-" + window + "-" + strconv.Itoa(delay), Name: "hb " + window, Type: spec.TypeMetric,
			Query:   "max(last_" + window + "):max:heartbeat{*} < 1",
			Options: spec.Options{EvaluationIntervalS: 30, EvaluationDelayS: i(delay), NotifyNoData: true, NoDataTimeframeM: 2},
			Message: "{{.Status}} @cap"}
	}
	short, long, delayed := h.create(silent("1m", 0)), h.create(silent("5m", 0)), h.create(silent("5m", 30))
	// A heartbeat every 10s whose last point is at 12:04:00.
	h.points("heartbeat", nil, 0, 4*time.Minute+time.Second, func(time.Duration) float64 { return 1 })
	for off := 30 * time.Second; off <= 12*time.Minute; off += 30 * time.Second {
		h.evalAt(t0.Add(off))
	}
	// The timeframe decides: two minutes after the last point.
	eqStrings(t, "1m window", h.transitions(short), []string{"12:06:00 transition OK>NO_DATA"})
	// The window decides: the timeframe has passed long before the five-minute
	// window stops seeing the last point. The window includes its start, so at
	// 12:09:00 it still holds the 12:04:00 point; 12:09:30 is the first without.
	eqStrings(t, "5m window", h.transitions(long), []string{"12:09:30 transition OK>NO_DATA"})
	// Both are measured to the window's end, evaluation_delay_s before the
	// tick, so a delay adds itself.
	eqStrings(t, "5m window, 30s delay", h.transitions(delayed), []string{"12:10:00 transition OK>NO_DATA"})
}

// TestBacktestMatchesLive is the acceptance criterion "the editor's backtest
// markers match what the live evaluator then does": the scheduler, driven by
// its own Run loop on a fake clock (real phase offset, real worker pool), and a
// backtest over the same range record the same transitions at the same times.
func TestBacktestMatchesLive(t *testing.T) {
	testutil.CheckGoroutines(t)
	h := newHarness(t)
	m := backlog()
	id := h.create(m)
	scenarioTimeline(h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.s.Run(ctx) }()
	testutil.Eventually(t, 5*time.Second, func() bool { return h.clk.Waiters() >= 2 }, "scheduler armed")
	for step := 1; step <= 36; step++ {
		h.clk.Advance(30 * time.Second)
		want := int64(step)
		testutil.Eventually(t, 5*time.Second, func() bool { return h.s.evaluations.Value() >= want }, "evaluation %d", step)
		testutil.Eventually(t, 5*time.Second, func() bool {
			h.s.mu.Lock()
			defer h.s.mu.Unlock()
			for _, e := range h.s.entries {
				if e.running {
					return false
				}
			}
			return true
		}, "evaluation %d done", step)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	live := h.transitions(id)
	c, _ := spec.Compile(m)
	bt, err := h.s.Backtest(context.Background(), c, id, t0, t0.Add(18*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var back []string
	for _, e := range bt.Events {
		back = append(back, e.Ts.UTC().Format("15:04:05")+" transition "+e.From+">"+e.To)
	}
	eqStrings(t, "backtest vs live", back, live)
	if len(live) != 5 {
		t.Fatalf("live run recorded %d transitions: %v", len(live), live)
	}
	if bt.Final["*"] != "OK" || bt.Evaluations != 36 {
		t.Errorf("final %v after %d", bt.Final, bt.Evaluations)
	}
	// The line is what each evaluation compared: one point per evaluation,
	// and at every marker the line's value is the event's.
	if len(bt.Series) != 1 || len(bt.Series[0].Points) != bt.Evaluations || bt.SeriesOmitted != 0 {
		t.Fatalf("series %d lines, omitted %d", len(bt.Series), bt.SeriesOmitted)
	}
	at := map[int64]float64{}
	for _, p := range bt.Series[0].Points {
		at[p.Ts.Unix()] = p.Value
	}
	for _, e := range bt.Events {
		v, ok := at[e.Ts.Unix()]
		if !ok || (e.Value == nil) != math.IsNaN(v) || (e.Value != nil && v != *e.Value) {
			t.Errorf("event at %v has value %v, line has %v", e.Ts, e.Value, v)
		}
	}
}

// TestRestartMidAlertSendsNoDuplicate is L5: a new scheduler over the same
// database, mid-ALERT, sends nothing until renotify is due, and then renotifies
// on the original schedule.
func TestRestartMidAlertSendsNoDuplicate(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Options.RenotifyIntervalM = 5
	m.Message = "@cap"
	h.create(m)
	h.points("queue.depth", nil, 0, 30*time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("first alert: %d notifications", n)
	}
	// Restart: a new scheduler, loading from the database.
	h.s = h.scheduler()
	for off := time.Minute; off < 5*time.Minute+30*time.Second; off += 30 * time.Second {
		h.evalAt(t0.Add(off))
	}
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("restart re-sent: %d notifications", n)
	}
	h.evalAt(t0.Add(5*time.Minute + 30*time.Second))
	notes := h.out.notes(t)
	if len(notes) != 2 || notes[1].Reason != "renotify" {
		t.Fatalf("renotify after restart: %+v", notes)
	}
}

// TestCommitFailureSendsNothingAndConverges is L5: a failed write during a
// transition sends no notification, and the next evaluation makes and
// announces the transition once.
func TestCommitFailureSendsNothingAndConverges(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Message = "@cap"
	id := h.create(m)
	h.points("queue.depth", nil, 0, 10*time.Minute, func(time.Duration) float64 { return 25 })
	h.store.fail(true)
	h.evalAt(t0.Add(30 * time.Second))
	h.evalAt(t0.Add(time.Minute))
	if n := len(h.out.notes(t)); n != 0 {
		t.Fatalf("notified without a persisted state: %d", n)
	}
	if st, _ := h.db.States(context.Background(), id); len(st) != 0 {
		t.Fatalf("state written: %v", st)
	}
	h.store.fail(false)
	h.evalAt(t0.Add(90 * time.Second))
	h.evalAt(t0.Add(2 * time.Minute))
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("after recovery: %d notifications", n)
	}
	eqStrings(t, "transitions", h.transitions(id), []string{"12:01:30 transition OK>ALERT"})
}

type erroring struct {
	Evaluator
	mu  sync.Mutex
	err error
}

func (e *erroring) Evaluate(ctx context.Context, c *spec.Compiled, now time.Time) ([]eval.Value, eval.Data, error) {
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return nil, eval.Data{}, err
	}
	return e.Evaluator.Evaluate(ctx, c, now)
}

// TestQueryErrorLeavesStateAlone is L5: a timeout is an eval_error event,
// recorded once, with the state unchanged; recovery is recorded too.
func TestQueryErrorLeavesStateAlone(t *testing.T) {
	h := newHarness(t)
	er := &erroring{Evaluator: h.ev}
	h.ev = er
	h.s = h.scheduler()
	m := backlog()
	m.Message = "@cap"
	id := h.create(m)
	h.points("queue.depth", nil, 0, 10*time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	er.err = context.DeadlineExceeded
	for off := time.Minute; off <= 3*time.Minute; off += 30 * time.Second {
		h.evalAt(t0.Add(off))
	}
	er.err = nil
	h.evalAt(t0.Add(4 * time.Minute))
	evs, _ := h.db.Events(context.Background(), meta.EventQuery{MonitorID: id})
	var kinds []string
	for i := len(evs) - 1; i >= 0; i-- {
		kinds = append(kinds, evs[i].Kind)
	}
	eqStrings(t, "events", kinds, []string{EventTransition, EventEvalError, EventEvalOK})
	if !strings.Contains(evs[1].Text, "timeout") {
		t.Errorf("error text %q", evs[1].Text)
	}
	st, _ := h.db.States(context.Background(), id)
	if len(st) != 1 || st[0].Status != "ALERT" || !st[0].LastEval.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("state %+v", st)
	}
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("notifications %d", n)
	}
}

func TestMuteSuppressesNotificationsNotState(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Message = "@cap"
	id := h.create(m)
	if _, err := h.db.SetMute(context.Background(), id, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.points("queue.depth", nil, 0, 10*time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	if n := len(h.out.notes(t)); n != 0 {
		t.Fatalf("muted monitor notified %d", n)
	}
	eqStrings(t, "transitions", h.transitions(id), []string{"12:00:30 transition OK>ALERT"})
}

func TestQueryChangeResetsGroups(t *testing.T) {
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
	for off := time.Minute; off <= 5*time.Minute; off += 30 * time.Second {
		h.evalAt(t0.Add(off))
	}
	// The old group did not go NO_DATA: it was forgotten.
	if st, _ := h.db.States(context.Background(), id); len(st) != 0 {
		t.Fatalf("states %v", st)
	}
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("notifications %d", n)
	}
}

func TestGroupExpires(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Options.NotifyNoData, m.Options.NoDataTimeframeM = false, 0
	id := h.create(m)
	h.points("queue.depth", nil, 0, time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	h.evalAt(t0.Add(GroupTTL))
	if st, _ := h.db.States(context.Background(), id); len(st) != 1 {
		t.Fatalf("expired early: %v", st)
	}
	h.evalAt(t0.Add(GroupTTL + time.Minute))
	if st, _ := h.db.States(context.Background(), id); len(st) != 0 {
		t.Fatalf("not expired: %v", st)
	}
	evs, _ := h.db.Events(context.Background(), meta.EventQuery{MonitorID: id, Kinds: []string{EventGroupExpired}})
	if len(evs) != 1 || evs[0].From != "ALERT" {
		t.Fatalf("expiry event %+v", evs)
	}
}

func TestDisabledMonitorsAreNotScheduledAndKeepState(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Message = "@cap"
	id := h.create(m)
	h.points("queue.depth", nil, 0, 10*time.Minute, func(time.Duration) float64 { return 25 })
	h.evalAt(t0.Add(30 * time.Second))
	row, _ := h.db.Monitor(context.Background(), id)
	row.Enabled = false
	_, _ = h.db.UpdateMonitor(context.Background(), id, row, t0)
	h.evalAt(t0.Add(time.Minute))
	if len(h.s.entries) != 0 {
		t.Fatal("disabled monitor scheduled")
	}
	row.Enabled = true
	_, _ = h.db.UpdateMonitor(context.Background(), id, row, t0)
	h.evalAt(t0.Add(90 * time.Second))
	// Re-enabled from its persisted ALERT: no second announcement.
	if n := len(h.out.notes(t)); n != 1 {
		t.Fatalf("re-enable re-announced: %d", n)
	}
}

func TestLogMonitorZeroFillRecovers(t *testing.T) {
	h := newHarness(t)
	ls := &fixedLogs{}
	h.ev = &eval.Evaluator{Logs: ls}
	h.s = h.scheduler()
	id := h.create(spec.Monitor{Name: "errors", Type: spec.TypeLog,
		Query:   `logs("status:error").rollup("count").by("service").last("1m") > 2`,
		Options: spec.Options{EvaluationDelayS: i(0)}, Message: "@cap"})
	ls.set(map[string]int64{"api": 5})
	h.evalAt(t0.Add(30 * time.Second))
	ls.set(nil) // the group stops logging errors: a count of zero, not silence
	h.evalAt(t0.Add(time.Minute))
	eqStrings(t, "transitions", h.transitions(id), []string{
		"12:00:30 transition OK>ALERT",
		"12:01:00 transition ALERT>OK",
	})
}

// TestTestNotificationAndDryRun covers the two editor helpers.
func TestTestNotificationAndDryRun(t *testing.T) {
	h := newHarness(t)
	got := &recording{}
	h.s = New(Options{Store: h.store, Evaluator: h.ev, Clock: h.clk, Notifiers: map[string]notify.Notifier{"cap": got}})
	m := backlog()
	m.Message = "{{.Monitor}} test @cap @ghost"
	id := h.create(m)
	row, _ := h.db.Monitor(context.Background(), id)
	res, err := h.s.TestNotification(context.Background(), row)
	if err != nil || res["cap"] != nil || res["ghost"] == nil {
		t.Fatalf("%v %v", res, err)
	}
	if len(got.notes) != 1 || got.notes[0].Reason != "test" || got.notes[0].Status != "ALERT" {
		t.Fatalf("%+v", got.notes)
	}
	row.Message = "nobody"
	if _, err := h.s.TestNotification(context.Background(), row); !errors.Is(err, ErrNoMentions) {
		t.Fatalf("no mentions: %v", err)
	}
	h.points("queue.depth", []string{"q:a"}, 0, time.Minute, func(time.Duration) float64 { return 12 })
	h.points("queue.depth", []string{"q:b"}, 0, time.Minute, func(time.Duration) float64 { return 30 })
	c, _ := spec.Compile(spec.Monitor{Name: "x", Type: spec.TypeMetric, Query: "last(last_1m):avg:queue.depth{*} by {q} > 20",
		Options: spec.Options{EvaluationDelayS: i(0), Thresholds: spec.Thresholds{Warning: f(10)}}})
	vals, err := h.s.DryRun(context.Background(), c, t0.Add(time.Minute))
	if err != nil || len(vals) != 2 || vals[0].Status != "WARN" || vals[1].Status != "ALERT" || *vals[1].Value != 30 {
		t.Fatalf("dry run %+v %v", vals, err)
	}
}

type recording struct{ notes []notify.Notification }

func (r *recording) Notify(_ context.Context, n notify.Notification) error {
	r.notes = append(r.notes, n)
	return nil
}

func TestOffsetAndNextAfter(t *testing.T) {
	a, b := Offset("a", 0, 30*time.Second), Offset("b", 0, 30*time.Second)
	if a == b && Offset("c", 0, 30*time.Second) == a {
		t.Error("offsets do not spread")
	}
	if Offset("a", 0, 30*time.Second) != a || a >= 30*time.Second || a%time.Second != 0 {
		t.Errorf("offset %v", a)
	}
	if Offset("", 7, time.Minute) != Offset("7", 0, time.Minute) {
		t.Error("id fallback")
	}
	n := nextAfter(t0, 30*time.Second, 7*time.Second)
	if !n.Equal(t0.Add(7*time.Second)) || !nextAfter(n, 30*time.Second, 7*time.Second).Equal(t0.Add(37*time.Second)) {
		t.Errorf("next %v", n)
	}
}

func TestBacktestBounds(t *testing.T) {
	h := newHarness(t)
	c, _ := spec.Compile(backlog())
	if _, err := h.s.Backtest(context.Background(), c, 1, t0, t0); err == nil {
		t.Error("empty range")
	}
	if _, err := h.s.Backtest(context.Background(), c, 1, t0, t0.Add(8*24*time.Hour)); err == nil {
		t.Error("over range")
	}
}

// fixedLogs answers every aggregation with one bucket of the given counts.
type fixedLogs struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (f *fixedLogs) set(c map[string]int64) {
	f.mu.Lock()
	f.counts = c
	f.mu.Unlock()
}

func (f *fixedLogs) Aggregate(_ context.Context, _ logql.Node, from, _ int64, _ logstore.AggSpec) (*logstore.AggResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.counts) == 0 {
		return &logstore.AggResult{}, nil
	}
	return &logstore.AggResult{Buckets: []logstore.Bucket{{Ts: from, Counts: f.counts}}}, nil
}

// TestRequireFullWindowSkipLeavesStateAlone: a window with a gap says nothing
// about the value, so the group is not stepped — it neither recovers nor ages.
func TestRequireFullWindowSkipLeavesStateAlone(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "avg(last_1m):avg:queue.depth{*} > 20"
	m.Options.RequireFullWindow = true
	id := h.create(m)
	h.points("queue.depth", nil, 0, time.Minute, func(time.Duration) float64 { return 25 })
	// From 1m: low values with a gap in every window.
	h.points("queue.depth", nil, time.Minute, 5*time.Minute, func(off time.Duration) float64 {
		if (off/(10*time.Second))%3 == 0 {
			return math.NaN()
		}
		return 1
	})
	h.evalAt(t0.Add(time.Minute))
	h.evalAt(t0.Add(3 * time.Minute))
	st, _ := h.db.States(context.Background(), id)
	if len(st) != 1 || st[0].Status != "ALERT" || !st[0].LastEval.Equal(t0.Add(time.Minute)) {
		t.Fatalf("skipped window moved the state: %+v", st)
	}
}
