package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	qeval "github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
)

// NativeStep is the agent's flush interval: the finest bucket a monitor reads.
const NativeStep = 10 * time.Second

// maxBucketsPerFetch keeps each underlying query under the metric
// evaluator's bucket limit, with room for its inclusive end bucket.
const maxBucketsPerFetch = qeval.MaxBuckets - 10

// MaxCells bounds one Fetch's series × buckets: 4M float64s, 32 MB. A live
// evaluation of a day at its step is under 1,500 buckets, so it reaches this
// only with thousands of groups. A backtest of a week at ten seconds is
// 60,000 buckets a series, so the bound is what stops one request from
// holding gigabytes; checked before each series is allocated, not after.
const MaxCells = 4_000_000

// NoneGroup is the value a grouped log monitor gives logs that lack the
// group-by field. Spelled out rather than empty, because `service:` is not a
// tag a reader can make sense of.
const NoneGroup = "(none)"

// MetricEvaluator runs metricql. Implemented by
// internal/query/metricql/eval.Evaluator.
type MetricEvaluator interface {
	Eval(ctx context.Context, req qeval.Request) (qeval.Result, error)
}

// LogAggregator counts logs. Implemented by internal/logstore.Store.
type LogAggregator interface {
	Aggregate(ctx context.Context, q logql.Node, from, to int64, a logstore.AggSpec) (*logstore.AggResult, error)
}

// Evaluator evaluates monitors against the stores. Either store may be nil,
// in which case monitors of that type fail to evaluate with a reason.
type Evaluator struct {
	Metrics MetricEvaluator
	Logs    LogAggregator
	// LogScanBudget is passed to every log aggregation.
	LogScanBudget int64
}

// Series is one group's buckets over a fetched range. Values[i] covers
// [Start + i*Step, Start + (i+1)*Step); NaN is an empty bucket.
type Series struct {
	Group  spec.Group
	Start  time.Time
	Step   time.Duration
	Values []float64
}

// Data is a fetched range: every group's buckets on one grid.
type Data struct {
	From, To time.Time
	Step     time.Duration
	Series   map[string]*Series // by Group.String()
	// AbsentIsZero is true for log monitors: a group with no matching line in
	// the window counted zero, it did not go silent.
	AbsentIsZero bool
}

// Value is one group's reduced value for one evaluation.
type Value struct {
	Key   string
	Group spec.Group
	// Present is false when the group had no point in the window — or, with
	// require_full_window, when it had a gap. Skipped tells the two apart.
	Present bool
	Skipped bool
	Value   float64
	// DataTs is the start of the newest non-empty bucket.
	DataTs time.Time
}

// Window is the range one evaluation at now reads: it ends `delay` before now,
// aligned down to the step so consecutive evaluations read whole buckets.
//
// Aligned, not exact, for the same reason dashboards align: a bucket the
// window cuts in half reduces to a value that depends on where the cut fell,
// and the backtest — which slides over buckets — could not reproduce it.
func Window(c *spec.Compiled, now time.Time) (from, to time.Time) {
	step := Step(c)
	to = now.Add(-c.Delay).Truncate(step)
	return to.Add(-c.Parsed.Window), to
}

// Step is the bucket width a monitor is evaluated on: the agent's flush
// interval, an explicit `.rollup(_, seconds)` written in the query, or — for a
// window long enough that ten-second buckets would exceed the evaluator's
// limit — the smallest multiple of ten seconds that fits and divides both the
// window and a day.
func Step(c *spec.Compiled) time.Duration {
	if c.Parsed.Metric != nil {
		var s int64
		metricql.Walk(c.Parsed.Metric.Expr, func(n metricql.Node) {
			if q, ok := n.(*metricql.Query); ok {
				for _, m := range q.Modifiers {
					if m.Kind == metricql.ModRollup && m.Seconds > 0 {
						s = m.Seconds
					}
				}
			}
		})
		if s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	// A step must divide the window and a day, or the aligned window holds a
	// partial bucket the store's epoch-floored buckets do not line up with.
	// A window is whole minutes, so 60 s always qualifies and the loop ends.
	step := NativeStep
	for c.Parsed.Window/step > maxBucketsPerFetch || c.Parsed.Window%step != 0 || (24*time.Hour)%step != 0 {
		step += NativeStep
	}
	return step
}

// Evaluate runs one evaluation at now and reduces every group.
func (e *Evaluator) Evaluate(ctx context.Context, c *spec.Compiled, now time.Time) ([]Value, Data, error) {
	from, to := Window(c, now)
	d, err := e.Fetch(ctx, c, from, to)
	if err != nil {
		return nil, Data{}, err
	}
	return Reduce(c, d, from, to), d, nil
}

// Fetch reads every group's buckets over [from, to). Long ranges are fetched
// in pieces, each under the query evaluator's bucket limit, so the backtest
// can read a day at the step a live evaluation uses.
func (e *Evaluator) Fetch(ctx context.Context, c *spec.Compiled, from, to time.Time) (Data, error) {
	step := Step(c)
	d := Data{From: from, To: to, Step: step, Series: map[string]*Series{}}
	if !to.After(from) {
		return d, fmt.Errorf("monitor eval: empty range %v..%v", from, to)
	}
	if c.Parsed.Log != nil {
		d.AbsentIsZero = true
		return d, e.fetchLogs(ctx, c, &d)
	}
	if e.Metrics == nil {
		return d, errors.New("monitor eval: this server has no metric store")
	}
	n := int(to.Sub(from) / step)
	for lo := 0; lo < n; lo += maxBucketsPerFetch {
		hi := min(lo+maxBucketsPerFetch, n)
		pf, pt := from.Add(time.Duration(lo)*step), from.Add(time.Duration(hi)*step)
		res, err := e.Metrics.Eval(ctx, qeval.Request{
			Expr: c.Parsed.Metric.Expr,
			From: pf.Unix(),
			// The evaluator's window is inclusive of the bucket To falls in;
			// one second short keeps it to [pf, pt).
			To:       pt.Unix() - 1,
			Interval: int64(step / time.Second),
		})
		if err != nil {
			return d, err
		}
		for _, s := range res.Series {
			g := spec.Group(s.Tags)
			key := g.String()
			ser, err := d.series(key, g, n)
			if err != nil {
				return d, err
			}
			for _, p := range s.Points {
				i := int(time.UnixMilli(p.T).Sub(from) / step)
				if i < lo || i >= hi || math.IsNaN(p.V) {
					continue
				}
				// Two output lines with one scope (arithmetic between
				// ungrouped queries can do it): keep the first rather than
				// sum, which would invent a number nobody wrote.
				if math.IsNaN(ser.Values[i]) {
					ser.Values[i] = p.V
				}
			}
		}
	}
	return d, nil
}

func (d *Data) series(key string, g spec.Group, n int) (*Series, error) {
	s := d.Series[key]
	if s == nil {
		if (len(d.Series)+1)*n > MaxCells {
			return nil, fmt.Errorf("monitor eval: more than %d groups of %d buckets; use a shorter range or fewer groups",
				len(d.Series), n)
		}
		if g == nil {
			g = spec.Group{}
		}
		s = &Series{Group: g, Start: d.From, Step: d.Step, Values: make([]float64, n)}
		for i := range s.Values {
			s.Values[i] = math.NaN()
		}
		d.Series[key] = s
	}
	return s, nil
}

func (e *Evaluator) fetchLogs(ctx context.Context, c *spec.Compiled, d *Data) error {
	if e.Logs == nil {
		return errors.New("monitor eval: this server has no log store")
	}
	lq := c.Parsed.Log
	n := int(d.To.Sub(d.From) / d.Step)
	res, err := e.Logs.Aggregate(ctx, lq.Expr, d.From.UnixMilli(), d.To.UnixMilli()-1,
		logstore.AggSpec{Interval: d.Step, By: lq.By, ScanBudget: e.LogScanBudget})
	if err != nil {
		return err
	}
	if res.Truncated {
		// A partial count is a lower bound, and a monitor that silently
		// compares a lower bound against a threshold under-alerts exactly when
		// there are the most logs. Say so instead.
		return fmt.Errorf("monitor eval: the log query read past its scan budget; narrow the filter")
	}
	if lq.By == "" {
		// One series, present even when nothing matched: zero lines is a count.
		ser, err := d.series("*", spec.Group{}, n)
		if err != nil {
			return err
		}
		for i := range ser.Values {
			ser.Values[i] = 0
		}
	}
	for _, b := range res.Buckets {
		i := int(time.UnixMilli(b.Ts).Sub(d.From) / d.Step)
		if i < 0 || i >= n {
			continue
		}
		if _, over := b.Counts[logstore.OtherGroup]; over && lq.By != "" {
			// Past the store's group cap, which values get their own group
			// depends on scan order. A group that lands in "(other)" one
			// evaluation is absent, so zero, so recovered, and alerts again
			// the next: flapping, plus a group named "(other)". Refuse.
			return fmt.Errorf("monitor eval: %q has more than %d distinct values in the window; group by a field with fewer",
				lq.By, logstore.MaxGroups)
		}
		for gv, cnt := range b.Counts {
			g := spec.Group{}
			if lq.By != "" {
				if gv == "" {
					gv = NoneGroup
				}
				g[lq.By] = gv
			}
			ser, err := d.series(g.String(), g, n)
			if err != nil {
				return err
			}
			if math.IsNaN(ser.Values[i]) {
				ser.Values[i] = 0
			}
			ser.Values[i] += float64(cnt)
		}
	}
	// A grouped log series is zero, not empty, in the buckets where that
	// group had no line: counts are dense.
	for _, s := range d.Series {
		for i, v := range s.Values {
			if math.IsNaN(v) {
				s.Values[i] = 0
			}
		}
	}
	return nil
}

// Reduce computes every group's value over [from, to), which must lie on
// d's grid. It is the function both a live evaluation and a backtest call, so
// the two cannot disagree about a value.
func Reduce(c *spec.Compiled, d Data, from, to time.Time) []Value {
	lo := int(from.Sub(d.From) / d.Step)
	hi := int(to.Sub(d.From) / d.Step)
	keys := make([]string, 0, len(d.Series))
	for k := range d.Series {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Value, 0, len(keys))
	for _, k := range keys {
		s := d.Series[k]
		v := reduce(c, s, max(lo, 0), min(hi, len(s.Values)))
		v.Key, v.Group = k, s.Group
		out = append(out, v)
	}
	return out
}

func reduce(c *spec.Compiled, s *Series, lo, hi int) Value {
	agg := spec.AggSum // log monitors count
	if c.Parsed.Metric != nil {
		agg = c.Parsed.Metric.Agg
	}
	var v Value
	n, nonNull := hi-lo, 0
	var acc float64
	for i := lo; i < hi; i++ {
		x := s.Values[i]
		if math.IsNaN(x) {
			continue
		}
		v.DataTs = s.Start.Add(time.Duration(i) * s.Step)
		switch {
		case nonNull == 0:
			acc = x
		case agg == spec.AggMin:
			acc = math.Min(acc, x)
		case agg == spec.AggMax:
			acc = math.Max(acc, x)
		case agg == spec.AggLast:
			acc = x
		default: // avg, sum (pct_nonnull ignores acc)
			acc += x
		}
		nonNull++
	}
	if c.Options.RequireFullWindow && nonNull < n {
		v.Skipped = nonNull > 0
		return v
	}
	if agg == spec.AggPctNonNull {
		if n > 0 {
			v.Value, v.Present = 100*float64(nonNull)/float64(n), true
		}
		return v
	}
	if nonNull == 0 {
		return v
	}
	if agg == spec.AggAvg {
		acc /= float64(nonNull)
	}
	v.Value, v.Present = acc, true
	return v
}
