package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// TestPickSeries: groups with events come first, in the order of their
// first event, then the rest by key, up to the bound; the rest are counted.
func TestPickSeries(t *testing.T) {
	var keys []string
	for i := range MaxBacktestSeries + 5 {
		keys = append(keys, fmt.Sprintf("g%02d", MaxBacktestSeries+4-i)) // any order in
	}
	got, omitted := pickSeries(keys, []string{"g24", "g03", "g24", "gone"})
	if len(got) != MaxBacktestSeries || omitted != 5 {
		t.Fatalf("%d lines, %d omitted", len(got), omitted)
	}
	if got[0] != "g24" || got[1] != "g03" || got[2] != "g00" || got[3] != "g01" {
		t.Fatalf("order %v", got[:4])
	}
	if got, omitted := pickSeries(nil, nil); len(got) != 0 || omitted != 0 {
		t.Fatalf("empty: %v %d", got, omitted)
	}
}

// TestTickValueJSON is the wire shape: [unix seconds, value or null].
func TestTickValueJSON(t *testing.T) {
	ts := time.Unix(1790000000, 0)
	b, _ := json.Marshal([]TickValue{{Ts: ts, Value: 2.5}, {Ts: ts.Add(time.Minute), Value: math.NaN()}})
	if string(b) != `[[1790000000,2.5],[1790000060,null]]` {
		t.Fatal(string(b))
	}
}

// TestBacktestLinesHaveOnePointPerEvaluation: a group that first has data
// part-way through gets nulls before it, so every line lines up with the
// evaluations, and the line holds the reduced value, not the raw points.
func TestBacktestLinesHaveOnePointPerEvaluation(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	h.points("queue.depth", []string{"q:a"}, 0, 10*time.Minute, func(time.Duration) float64 { return 5 })
	h.points("queue.depth", []string{"q:b"}, 5*time.Minute, 10*time.Minute, func(off time.Duration) float64 {
		return float64(off / time.Minute) // 5..9: last() of the window, not an average
	})
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := h.s.Backtest(context.Background(), c, 1, t0, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bt.Series) != 2 {
		t.Fatalf("%d lines", len(bt.Series))
	}
	for _, s := range bt.Series {
		if len(s.Points) != bt.Evaluations {
			t.Errorf("%s: %d points for %d evaluations", s.Group, len(s.Points), bt.Evaluations)
		}
		if s.Group == "q:b" {
			if !math.IsNaN(s.Points[0].Value) {
				t.Errorf("q:b before its data: %v", s.Points[0].Value)
			}
			if last := s.Points[len(s.Points)-1].Value; last != 9 {
				t.Errorf("q:b last value %v, want 9 (the window's last point)", last)
			}
			if s.Tags["q"] != "b" {
				t.Errorf("tags %v", s.Tags)
			}
		}
	}
}

// TestBacktestLineHasNullsWhereAWindowWasSkipped: with require_full_window, a
// window with a gap is not evaluated for the group, and the line says so with
// a null rather than dropping the point.
func TestBacktestLineHasNullsWhereAWindowWasSkipped(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Options.RequireFullWindow = true
	h.points("queue.depth", nil, 0, 10*time.Minute, func(off time.Duration) float64 {
		if off >= 5*time.Minute && off < 6*time.Minute {
			return math.NaN() // a minute-long gap
		}
		return 5
	})
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := h.s.Backtest(context.Background(), c, 1, t0, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bt.Series) != 1 || len(bt.Series[0].Points) != bt.Evaluations {
		t.Fatalf("lines %+v for %d evaluations", bt.Series, bt.Evaluations)
	}
	nulls := 0
	for _, p := range bt.Series[0].Points[3:] { // past the first, partial windows
		if math.IsNaN(p.Value) {
			nulls++
		}
	}
	if nulls == 0 {
		t.Fatal("no null where the window had a gap")
	}
}

// TestBacktestKeepsTheGroupsWithEvents: past MaxBacktestSeries groups, the
// one that alerted is kept (and first) even though it sorts last, and its
// line is recorded in full.
func TestBacktestKeepsTheGroupsWithEvents(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	for i := range MaxBacktestSeries + 3 {
		h.points("queue.depth", []string{fmt.Sprintf("q:a%02d", i)}, 0, 10*time.Minute, func(time.Duration) float64 { return 1 })
	}
	h.points("queue.depth", []string{"q:zz"}, 0, 10*time.Minute, func(off time.Duration) float64 {
		if off >= 5*time.Minute {
			return 50
		}
		return 1
	})
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := h.s.Backtest(context.Background(), c, 1, t0, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bt.Series) != MaxBacktestSeries || bt.SeriesOmitted != 4 {
		t.Fatalf("%d lines, %d omitted", len(bt.Series), bt.SeriesOmitted)
	}
	z := bt.Series[0]
	if z.Group != "q:zz" || len(z.Points) != bt.Evaluations || z.Points[len(z.Points)-1].Value != 50 {
		t.Fatalf("first line %s, %d points, last %v", z.Group, len(z.Points), z.Points[len(z.Points)-1])
	}
	if bt.Series[1].Group != "q:a00" || bt.Series[1].Points[len(z.Points)-1].Value != 1 {
		t.Fatalf("second line %+v", bt.Series[1].Group)
	}
}

// TestBacktestCountsEventsPastTheCap: past MaxBacktestEvents the events are
// counted, not kept, and the totals still cover them all.
func TestBacktestCountsEventsPastTheCap(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	m.Options.Thresholds.Warning, m.Options.Thresholds.WarningRecovery = nil, nil
	flap := func(off time.Duration) float64 {
		if (off/(30*time.Second))%2 == 0 {
			return 50
		}
		return 1
	}
	for i := range 300 {
		h.points("queue.depth", []string{fmt.Sprintf("q:g%03d", i)}, 0, 10*time.Minute, flap)
	}
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := h.s.Backtest(context.Background(), c, 1, t0, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bt.Events) != MaxBacktestEvents || bt.EventsOmitted == 0 {
		t.Fatalf("%d events kept, %d omitted", len(bt.Events), bt.EventsOmitted)
	}
	if bt.Transitions != len(bt.Events)+bt.EventsOmitted || bt.Notifications == 0 {
		t.Fatalf("transitions %d, kept+omitted %d, notifications %d", bt.Transitions, len(bt.Events)+bt.EventsOmitted, bt.Notifications)
	}
}

// TestBacktestLinesMatchAFullReplay pins what the second pass relies on:
// groups step independently, so replaying only the shown groups' data gives
// each of them the values a replay of everything does, at every tick.
func TestBacktestLinesMatchAFullReplay(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "avg(last_1m):avg:queue.depth{*} by {q} > 20"
	m.Options.RequireFullWindow = true
	for i := range MaxBacktestSeries + 5 {
		h.points("queue.depth", []string{fmt.Sprintf("q:g%02d", i)}, time.Duration(i)*20*time.Second, 15*time.Minute, func(off time.Duration) float64 {
			if (off/time.Minute+time.Duration(i))%7 == 3 {
				return math.NaN() // gaps, so some windows are skipped
			}
			return float64((int(off/(10*time.Second)) * (i + 3)) % 40)
		})
	}
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	from, to := t0, t0.Add(15*time.Minute)
	bt, err := h.s.Backtest(context.Background(), c, 1, from, to)
	if err != nil {
		t.Fatal(err)
	}
	first := nextAfter(from.Add(-time.Nanosecond), c.Interval, Offset(c.UID, 1, c.Interval))
	ff, _ := eval.Window(c, first)
	_, ft := eval.Window(c, to)
	data, err := h.s.opt.Evaluator.Fetch(context.Background(), c, ff, ft)
	if err != nil {
		t.Fatal(err)
	}
	full := map[string]map[int]float64{}
	if _, _, err := replay(context.Background(), c, data, first, to, func(i int, _ time.Time, st groupStep) {
		if full[st.Key] == nil {
			full[st.Key] = map[int]float64{}
		}
		full[st.Key][i] = st.Value
	}); err != nil {
		t.Fatal(err)
	}
	if len(bt.Series) != MaxBacktestSeries || len(bt.Events) == 0 {
		t.Fatalf("%d lines, %d events", len(bt.Series), len(bt.Events))
	}
	for _, s := range bt.Series {
		for i, p := range s.Points {
			want, ok := full[s.Group][i]
			if !ok {
				want = math.NaN()
			}
			if !(p.Value == want || (math.IsNaN(p.Value) && math.IsNaN(want))) {
				t.Fatalf("%s tick %d: line %v, full replay %v", s.Group, i, p.Value, want)
			}
		}
	}
}

// TestBacktestLinesFavourGroupsPastTheEventCap: five flapping groups fill
// MaxBacktestEvents, then one more group alerts. Its events are all past the
// cap, and it still gets a line ahead of the quiet groups.
func TestBacktestLinesFavourGroupsPastTheEventCap(t *testing.T) {
	h := newHarness(t)
	m := backlog()
	m.Query = "last(last_1m):avg:queue.depth{*} by {q} > 20"
	m.Options.Thresholds.Warning, m.Options.Thresholds.WarningRecovery = nil, nil
	span := 1100 * 30 * time.Second
	for i := range 5 {
		h.points("queue.depth", []string{fmt.Sprintf("q:flap%d", i)}, 0, span, func(off time.Duration) float64 {
			if (off/(30*time.Second))%2 == 0 {
				return 50
			}
			return 1
		})
	}
	for i := range MaxBacktestSeries {
		h.points("queue.depth", []string{fmt.Sprintf("q:a%02d", i)}, 0, span, func(time.Duration) float64 { return 1 })
	}
	h.points("queue.depth", []string{"q:zlate"}, 0, span, func(off time.Duration) float64 {
		if off > span-20*time.Minute {
			return 50
		}
		return 1
	})
	c, err := spec.Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := h.s.Backtest(context.Background(), c, 1, t0, t0.Add(span))
	if err != nil {
		t.Fatal(err)
	}
	if bt.EventsOmitted == 0 || slices.ContainsFunc(bt.Events, func(e BacktestEvent) bool { return e.Group == "q:zlate" }) {
		t.Fatalf("setup: the late group's events should all be past the cap (omitted %d)", bt.EventsOmitted)
	}
	if !slices.ContainsFunc(bt.Series, func(s BacktestSeries) bool { return s.Group == "q:zlate" }) {
		t.Fatal("the group that alerted past the cap has no line")
	}
}
