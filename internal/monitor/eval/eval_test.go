package eval

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
	qeval "github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func compile(t *testing.T, typ spec.Type, q string, mod ...func(*spec.Monitor)) *spec.Compiled {
	t.Helper()
	m := spec.Monitor{Name: "m", Type: typ, Query: q}
	for _, f := range mod {
		f(&m)
	}
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func store(t *testing.T) *naive.Store {
	t.Helper()
	s, err := naive.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// put writes value v at each of the given offsets from t0 for one series.
func put(t *testing.T, s tsdb.MetricStore, metric string, tags []string, pts map[time.Duration]float64) {
	t.Helper()
	var smp []tsdb.Sample
	for off := -48 * time.Hour; off <= 48*time.Hour; off += 10 * time.Second {
		if v, ok := pts[off]; ok {
			smp = append(smp, tsdb.Sample{T: t0.Add(off).UnixMilli(), V: v})
		}
	}
	if _, err := s.Append(context.Background(), []tsdb.SeriesSamples{{Series: tsdb.NewSeriesRef(metric, tags), Samples: smp}}); err != nil {
		t.Fatal(err)
	}
}

func evaluator(s tsdb.MetricStore) *Evaluator {
	return &Evaluator{Metrics: &qeval.Evaluator{Store: s, Timeout: -1}}
}

func TestWindowIsDelayedAndAligned(t *testing.T) {
	c := compile(t, spec.TypeMetric, "avg(last_5m):avg:x{*} > 1")
	from, to := Window(c, t0.Add(37*time.Second))
	// now 12:00:37, delay 30s → 12:00:07, aligned down to 12:00:00.
	if !to.Equal(t0) || !from.Equal(t0.Add(-5*time.Minute)) {
		t.Fatalf("window %v..%v", from, to)
	}
}

func TestStep(t *testing.T) {
	if s := Step(compile(t, spec.TypeMetric, "avg(last_5m):avg:x{*} > 1")); s != NativeStep {
		t.Errorf("5m: %v", s)
	}
	if s := Step(compile(t, spec.TypeMetric, "avg(last_24h):avg:x{*} > 1")); s != time.Minute {
		t.Errorf("24h: %v", s)
	}
	if s := Step(compile(t, spec.TypeMetric, "avg(last_1h):avg:x{*}.rollup(max, 60) > 1")); s != time.Minute {
		t.Errorf("explicit rollup: %v", s)
	}
}

func TestReducers(t *testing.T) {
	s := store(t)
	// Window is t0-1m..t0 (delay 0): six buckets, two empty.
	put(t, s, "x", nil, map[time.Duration]float64{
		-60 * time.Second: 4, -50 * time.Second: 2, -30 * time.Second: 9, -10 * time.Second: 5,
		// Outside the window on both sides: must not count.
		-70 * time.Second: 1000, 0: 1000,
	})
	e := evaluator(s)
	for _, c := range []struct {
		agg  string
		want float64
	}{
		{"avg", 5}, {"sum", 20}, {"min", 2}, {"max", 9}, {"last", 5}, {"pct_nonnull", 100 * 4.0 / 6},
	} {
		m := compile(t, spec.TypeMetric, c.agg+"(last_1m):avg:x{*} > 1000", func(m *spec.Monitor) {
			m.Options.EvaluationDelayS = new(int)
		})
		vals, _, err := e.Evaluate(context.Background(), m, t0)
		if err != nil {
			t.Fatal(err)
		}
		if len(vals) != 1 || !vals[0].Present || math.Abs(vals[0].Value-c.want) > 1e-9 || vals[0].Key != "*" {
			t.Errorf("%s: %+v, want %g", c.agg, vals, c.want)
		}
		if !vals[0].DataTs.Equal(t0.Add(-10 * time.Second)) {
			t.Errorf("%s: data ts %v", c.agg, vals[0].DataTs)
		}
	}
	// require_full_window skips a window with a gap.
	m := compile(t, spec.TypeMetric, "avg(last_1m):avg:x{*} > 1", func(m *spec.Monitor) {
		m.Options.EvaluationDelayS, m.Options.RequireFullWindow = new(int), true
	})
	vals, _, err := e.Evaluate(context.Background(), m, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0].Present || !vals[0].Skipped {
		t.Errorf("full window: %+v", vals)
	}
}

func TestGroupsAndAbsence(t *testing.T) {
	s := store(t)
	put(t, s, "q", []string{"queue:a"}, map[time.Duration]float64{-40 * time.Second: 30, -50 * time.Second: 10})
	put(t, s, "q", []string{"queue:b"}, map[time.Duration]float64{-40 * time.Second: 1})
	// queue:c only has data long before the window: absent this evaluation.
	put(t, s, "q", []string{"queue:c"}, map[time.Duration]float64{-2 * time.Hour: 7})
	c := compile(t, spec.TypeMetric, "max(last_5m):avg:q{*} by {queue} > 20")
	vals, _, err := evaluator(s).Evaluate(context.Background(), c, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 || vals[0].Key != "queue:a" || vals[0].Value != 30 || vals[1].Key != "queue:b" || vals[1].Value != 1 {
		t.Fatalf("got %+v", vals)
	}
	if vals[0].Group["queue"] != "a" {
		t.Fatalf("group %v", vals[0].Group)
	}
}

func TestEmptyWindowIsAbsent(t *testing.T) {
	c := compile(t, spec.TypeMetric, "avg(last_5m):avg:nothing{*} > 1")
	vals, _, err := evaluator(store(t)).Evaluate(context.Background(), c, t0)
	if err != nil || len(vals) != 0 {
		t.Fatalf("%+v %v", vals, err)
	}
}

// TestFetchChunksAgreeWithLiveEvaluation: a long fetch is split into pieces,
// and reducing any window of it gives what evaluating at that time gives.
// That equality is what the backtest relies on.
func TestFetchChunksAgreeWithLiveEvaluation(t *testing.T) {
	s := store(t)
	pts := map[time.Duration]float64{}
	for off := -30 * time.Hour; off <= 0; off += 10 * time.Second {
		if (off/time.Second)%70 != 0 { // some gaps
			pts[off] = float64((off / time.Second) % 97)
		}
	}
	put(t, s, "w", []string{"k:v"}, pts)
	e := evaluator(s)
	for _, q := range []string{"avg(last_5m):avg:w{*} by {k} > 1", "max(last_24h):avg:w{*} by {k} > 1"} {
		c := compile(t, spec.TypeMetric, q)
		from, to := Window(c, t0.Add(-6*time.Hour))
		start := from.Add(-6 * time.Hour)
		d, err := e.Fetch(context.Background(), c, start, to)
		if err != nil {
			t.Fatal(err)
		}
		for now := t0.Add(-12 * time.Hour); !now.After(t0.Add(-6 * time.Hour)); now = now.Add(37 * time.Minute) {
			wf, wt := Window(c, now)
			got := Reduce(c, d, wf, wt)
			want, _, err := e.Evaluate(context.Background(), c, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) || (len(got) == 1 && (got[0].Value != want[0].Value || !got[0].DataTs.Equal(want[0].DataTs))) {
				t.Fatalf("%s at %v: sliding %+v, live %+v", q, now, got, want)
			}
		}
	}
}

type failingMetrics struct{ err error }

func (f failingMetrics) Eval(context.Context, qeval.Request) (qeval.Result, error) {
	return qeval.Result{}, f.err
}

func TestErrors(t *testing.T) {
	boom := errors.New("boom")
	c := compile(t, spec.TypeMetric, "avg(last_5m):avg:x{*} > 1")
	if _, _, err := (&Evaluator{Metrics: failingMetrics{boom}}).Evaluate(context.Background(), c, t0); !errors.Is(err, boom) {
		t.Errorf("metric error: %v", err)
	}
	if _, _, err := (&Evaluator{}).Evaluate(context.Background(), c, t0); err == nil {
		t.Error("no metric store")
	}
	lc := compile(t, spec.TypeLog, `logs("x").rollup("count").last("5m") > 1`)
	if _, _, err := (&Evaluator{}).Evaluate(context.Background(), lc, t0); err == nil {
		t.Error("no log store")
	}
	if _, err := (&Evaluator{}).Fetch(context.Background(), c, t0, t0); err == nil {
		t.Error("empty range")
	}
	if _, _, err := (&Evaluator{Logs: truncating{}}).Evaluate(context.Background(), lc, t0); err == nil {
		t.Error("truncated log count accepted")
	}
}

type truncating struct{}

func (truncating) Aggregate(context.Context, logql.Node, int64, int64, logstore.AggSpec) (*logstore.AggResult, error) {
	return &logstore.AggResult{Truncated: true}, nil
}

func logStore(t *testing.T, logs []wire.Log) *logstore.Store {
	t.Helper()
	ls, err := logstore.Open(logstore.Options{Dir: t.TempDir(), NoSync: true, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	if err := ls.Append(context.Background(), logs); err != nil {
		t.Fatal(err)
	}
	return ls
}

func TestLogMonitor(t *testing.T) {
	at := func(d time.Duration) int64 { return t0.Add(d).UnixMilli() }
	ls := logStore(t, []wire.Log{
		{Ts: at(-2 * time.Minute), Message: "boom", Status: "error", Service: "worker"},
		{Ts: at(-3 * time.Minute), Message: "boom", Status: "error", Service: "worker"},
		{Ts: at(-90 * time.Second), Message: "boom", Status: "error", Service: "api"},
		{Ts: at(-2 * time.Minute), Message: "fine", Status: "info", Service: "worker"},
		{Ts: at(-2 * time.Hour), Message: "old", Status: "error", Service: "worker"},
	})
	e := &Evaluator{Logs: ls}
	delay0 := func(m *spec.Monitor) { m.Options.EvaluationDelayS = new(int) }

	c := compile(t, spec.TypeLog, `logs("status:error").rollup("count").last("5m") > 2`, delay0)
	vals, d, err := e.Evaluate(context.Background(), c, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !d.AbsentIsZero || len(vals) != 1 || !vals[0].Present || vals[0].Value != 3 || vals[0].Key != "*" {
		t.Fatalf("ungrouped: %+v", vals)
	}
	// Nothing matching is a count of zero, present.
	c = compile(t, spec.TypeLog, `logs("status:critical").rollup("count").last("5m") > 2`, delay0)
	if vals, _, err = e.Evaluate(context.Background(), c, t0); err != nil || len(vals) != 1 || !vals[0].Present || vals[0].Value != 0 {
		t.Fatalf("zero: %+v %v", vals, err)
	}
	c = compile(t, spec.TypeLog, `logs("status:error").rollup("count").by("service").last("5m") > 1`, delay0)
	if vals, _, err = e.Evaluate(context.Background(), c, t0); err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 || vals[0].Key != "service:api" || vals[0].Value != 1 || vals[1].Key != "service:worker" || vals[1].Value != 2 {
		t.Fatalf("grouped: %+v", vals)
	}
	// A grouped log with no value for the field is "(none)", not an empty tag.
	c = compile(t, spec.TypeLog, `logs("status:error").rollup("count").by("@user").last("5m") > 1`, delay0)
	if vals, _, err = e.Evaluate(context.Background(), c, t0); err != nil || len(vals) != 1 || vals[0].Group["@user"] != NoneGroup {
		t.Fatalf("none group: %+v %v", vals, err)
	}
}

// TestGroupedLogCountsAreDense: a group's buckets without a line are zero,
// not gaps, so require_full_window does not skip a quiet group.
func TestGroupedLogCountsAreDense(t *testing.T) {
	ls := logStore(t, []wire.Log{{Ts: t0.Add(-2 * time.Minute).UnixMilli(), Message: "x", Status: "error", Service: "api"}})
	c := compile(t, spec.TypeLog, `logs("status:error").rollup("count").by("service").last("5m") > 1`, func(m *spec.Monitor) {
		m.Options.EvaluationDelayS, m.Options.RequireFullWindow = new(int), true
	})
	vals, _, err := (&Evaluator{Logs: ls}).Evaluate(context.Background(), c, t0)
	if err != nil || len(vals) != 1 || !vals[0].Present || vals[0].Value != 1 {
		t.Fatalf("%+v %v", vals, err)
	}
}

type twoLines struct{}

func (twoLines) Eval(_ context.Context, req qeval.Request) (qeval.Result, error) {
	p := func(v float64) []qeval.Point { return []qeval.Point{{T: (req.To - 9) * 1000, V: v}} }
	return qeval.Result{Series: []qeval.Series{{Tags: map[string]string{}, Points: p(1)}, {Tags: map[string]string{}, Points: p(100)}}}, nil
}

// TestDuplicateScopeKeepsFirst: two output lines with one scope are not summed.
func TestDuplicateScopeKeepsFirst(t *testing.T) {
	c := compile(t, spec.TypeMetric, "max(last_5m):avg:x{*} > 1")
	vals, _, err := (&Evaluator{Metrics: twoLines{}}).Evaluate(context.Background(), c, t0)
	if err != nil || len(vals) != 1 || vals[0].Value != 1 {
		t.Fatalf("%+v %v", vals, err)
	}
}
