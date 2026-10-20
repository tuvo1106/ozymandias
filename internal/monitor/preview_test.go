package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// TestPickSeries: groups with events come first, in the order of their
// first event, then the rest by key, up to the bound; the rest are counted.
func TestPickSeries(t *testing.T) {
	lines := map[string]*BacktestSeries{}
	for i := range MaxBacktestSeries + 5 {
		k := fmt.Sprintf("g%02d", i)
		lines[k] = &BacktestSeries{Group: k}
	}
	events := []BacktestEvent{{Group: "g24"}, {Group: "g03"}, {Group: "g24"}, {Group: "gone"}}
	got, omitted := pickSeries(lines, events)
	if len(got) != MaxBacktestSeries || omitted != 5 {
		t.Fatalf("%d lines, %d omitted", len(got), omitted)
	}
	if got[0].Group != "g24" || got[1].Group != "g03" || got[2].Group != "g00" || got[3].Group != "g01" {
		t.Fatalf("order %s %s %s %s", got[0].Group, got[1].Group, got[2].Group, got[3].Group)
	}
	if got, omitted := pickSeries(map[string]*BacktestSeries{}, nil); len(got) != 0 || omitted != 0 || got == nil {
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
