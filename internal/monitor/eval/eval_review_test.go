package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
)

// countsLogs answers every aggregate with one bucket of fixed counts.
type countsLogs map[string]int64

func (c countsLogs) Aggregate(_ context.Context, _ logql.Node, from, _ int64, _ logstore.AggSpec) (*logstore.AggResult, error) {
	_, over := c[overflowMark]
	counts := map[string]int64{}
	for k, v := range c {
		if k != overflowMark {
			counts[k] = v
		}
	}
	return &logstore.AggResult{Buckets: []logstore.Bucket{{Ts: from, Counts: counts}}, Overflowed: over}, nil
}

// overflowMark in a countsLogs makes its answer report an overflow.
const overflowMark = "\x00overflow"

// TestLogGroupCapIsAnError: past the store's group cap, which values get a
// group depends on scan order, so the evaluation refuses to answer.
func TestLogGroupCapIsAnError(t *testing.T) {
	c := compile(t, spec.TypeLog, `logs("x").rollup("count").by("@user").last("5m") > 1`)
	_, _, err := (&Evaluator{Logs: countsLogs{"u1": 3, logstore.OtherGroup: 9, overflowMark: 0}}).Evaluate(context.Background(), c, t0)
	if err == nil || !strings.Contains(err.Error(), "distinct values") {
		t.Fatalf("err %v", err)
	}
	// A log whose value really is "(other)" is a group like any other.
	vals, _, err := (&Evaluator{Logs: countsLogs{"u1": 3, logstore.OtherGroup: 2}}).Evaluate(context.Background(), c, t0)
	if err != nil || len(vals) != 2 {
		t.Fatal(vals, err)
	}
}

// TestFetchBoundsCells: a week at ten seconds is 60,480 buckets a group, so
// a few dozen groups reach MaxCells; the fetch stops before allocating more.
func TestFetchBoundsCells(t *testing.T) {
	c := compile(t, spec.TypeLog, `logs("x").rollup("count").by("service").last("1m") > 1`)
	counts := countsLogs{}
	for g := range 80 {
		counts[fmt.Sprint("s", g)] = 1
	}
	_, err := (&Evaluator{Logs: counts}).Fetch(context.Background(), c, t0.Add(-7*24*time.Hour), t0)
	if err == nil || !strings.Contains(err.Error(), "fewer groups") {
		t.Fatalf("err %v", err)
	}
	if _, err := (&Evaluator{Logs: counts}).Fetch(context.Background(), c, t0.Add(-time.Hour), t0); err != nil {
		t.Fatal(err)
	}
}

// TestFetchRefusesAStepThatDoesNotTile: past spec's validation (a hand-built
// window over 24h), Fetch errors rather than reading a misaligned grid.
func TestFetchRefusesAStepThatDoesNotTile(t *testing.T) {
	c := compile(t, spec.TypeLog, `logs("x").rollup("count").last("5m") > 1`)
	c.Parsed.Window = 1493 * time.Minute // coprime to 1440 minutes: only steps of at most 60s tile it, and 1493 of those exceed the limit
	if _, err := (&Evaluator{Logs: countsLogs{}}).Fetch(context.Background(), c, t0.Add(-time.Hour), t0); err == nil ||
		!strings.Contains(err.Error(), "no step fits") {
		t.Fatalf("err %v", err)
	}
}

// TestAutoStepDividesWindowAndDay: for every window the grammar allows, the
// chosen step tiles both the window and a day, within the bucket limit.
func TestAutoStepDividesWindowAndDay(t *testing.T) {
	for w := time.Minute; w <= spec.MaxWindow; w += time.Minute {
		c := &spec.Compiled{Parsed: spec.Query{Window: w}}
		st := Step(c)
		if w%st != 0 || (24*time.Hour)%st != 0 || w/st > maxBucketsPerFetch {
			t.Fatalf("window %v: step %v", w, st)
		}
	}
}
