package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Limits bound what one query may do. They exist because a query arrives from
// whoever can reach the API, and the expensive part is reading series, not
// parsing them.
const (
	// MaxSeriesPerNode bounds the series one query node may select. The error
	// names the two fixes — a narrower filter or a group-by — because the
	// caller cannot see how many series a metric has.
	MaxSeriesPerNode = 1000
	// MaxBuckets bounds the points per output line. No chart draws more, and
	// every bucket costs memory per group.
	MaxBuckets = 1500
	// DefaultTimeout bounds wall-clock. A query that has not answered in
	// thirty seconds is a query somebody has already given up on.
	DefaultTimeout = 30 * time.Second
)

// MetricTypes tells the planner each metric's type, which decides how its
// samples reduce over time. Implemented by internal/meta.
type MetricTypes interface {
	Metric(name string) (meta.Metric, bool)
}

// SketchReader is the part of internal/sketchstore the percentile path needs.
//
// Streaming, not a slice: the limit is on output buckets, and a week of one
// series at a ten-second flush is sixty thousand sketches. Each is folded into
// its bucket and dropped.
type SketchReader interface {
	ReadEach(ctx context.Context, ref tsdb.SeriesRef, fromMs, toMs int64, fn func(sketchstore.Point) error) error
}

// Evaluator answers queries against a set of stores. It holds no per-query
// state, so one is shared by every request.
type Evaluator struct {
	// Store serves series selection and samples.
	Store tsdb.MetricStore
	// Sketches serves the percentile aggregators. Nil means a percentile is
	// refused with a reason rather than answered from nothing.
	Sketches SketchReader
	// Types supplies metric types. Nil means every metric is untyped, which
	// is read as a gauge for time aggregation and refuses percentiles.
	Types MetricTypes
	// Timeout bounds one query; zero takes DefaultTimeout, negative means no
	// timeout (tests).
	Timeout time.Duration
}

// Request is one query to evaluate.
type Request struct {
	// Expr is the parsed query. Use [metricql.Parse].
	Expr metricql.Node
	// From and To are unix seconds; the window is [From, To].
	From, To int64
	// Interval is the bucket width in seconds. Zero lets the planner choose,
	// which is what a chart wants; an explicit value is what a monitor needs
	// so that its threshold means the same thing on every evaluation.
	Interval int64
	// Vars resolves the query's `$name` template variables. A variable may
	// expand to several matchers (`$env` -> `env:dev`), to one, or to none —
	// "no constraint", which is what a dashboard's "all" selection means.
	Vars map[string][]string
}

// Result is a query's answer.
type Result struct {
	From     int64    `json:"from"`
	To       int64    `json:"to"`
	Interval int64    `json:"interval"`
	Series   []Series `json:"series"`
	// Warnings are things the caller should know that did not stop the query:
	// groups dropped by a join, a variable that resolved to nothing. They are
	// for a human, so they are sentences.
	Warnings []string `json:"warnings"`
}

// Series is one output line.
type Series struct {
	// Metric is what the line came from: a metric name for a bare query, or
	// the expression's canonical text once arithmetic has combined two.
	Metric string `json:"metric"`
	// Tags holds the group-by keys and their values. A key that a group's
	// series did not carry is absent rather than empty.
	Tags   map[string]string `json:"tags"`
	Points []Point           `json:"points"`
}

// Scope renders a line's group the way a legend names it: the group-by tags,
// sorted, or "*" for a query with no grouping — the spelling for "everything,
// aggregated together".
func (s Series) Scope() string {
	if len(s.Tags) == 0 {
		return "*"
	}
	keys := make([]string, 0, len(s.Tags))
	for k := range s.Tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		keys[i] = wire.JoinTag(k, s.Tags[k])
	}
	return strings.Join(keys, ",")
}

// Point is [unix_ms, value]. An empty bucket is NaN here and null in JSON, so
// a chart draws a gap rather than a misleading zero.
type Point struct {
	T int64
	V float64
}

// MarshalJSON writes [t, v] or [t, null].
func (p Point) MarshalJSON() ([]byte, error) {
	b := strconv.AppendInt([]byte{'['}, p.T, 10)
	b = append(b, ',')
	if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
		b = append(b, "null"...)
	} else {
		b = strconv.AppendFloat(b, p.V, 'g', -1, 64)
	}
	return append(b, ']'), nil
}

// UnmarshalJSON reads [t, v|null]; null becomes NaN.
func (p *Point) UnmarshalJSON(data []byte) error {
	var pair [2]*float64
	if err := json.Unmarshal(data, &pair); err != nil || pair[0] == nil {
		return fmt.Errorf("point: want [ms, value|null]: %s", data)
	}
	p.T, p.V = int64(*pair[0]), math.NaN()
	if pair[1] != nil {
		p.V = *pair[1]
	}
	return nil
}

// ErrNoSketchStore means a percentile was asked of a deployment that stores no
// sketches. That is a server configuration answer, not a bad request.
var ErrNoSketchStore = errors.New("this server has no sketch store")

// Eval runs req.
//
// The stages are [Evaluator.plan], then a bottom-up walk of the expression.
// Every node is evaluated onto the one grid the plan chose, so that arithmetic
// between two nodes is pointwise without resampling anything.
func (e *Evaluator) Eval(ctx context.Context, req Request) (Result, error) {
	g, err := e.plan(req)
	if err != nil {
		return Result{}, err
	}
	timeout := e.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	st := &state{vars: req.Vars}
	f, err := e.node(ctx, req.Expr, g, st)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		From:     req.From,
		To:       req.To,
		Interval: g.interval,
		Series:   f.lines(req.Expr, g),
		Warnings: st.warnings,
	}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	return res, nil
}

// state is the per-query scratch the walk threads through: the variables to
// resolve against and the warnings collected on the way.
type state struct {
	vars     map[string][]string
	warnings []string
}

func (s *state) warnf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	// A join between two large results can drop many groups for one reason;
	// saying it once is the useful amount.
	if slices.Contains(s.warnings, msg) {
		return
	}
	s.warnings = append(s.warnings, msg)
}

// grid is the bucket layout every node in one query shares: n buckets of
// interval seconds, the first starting at first.
type grid struct {
	first    int64
	interval int64
	n        int
}

// at returns the unix-millisecond timestamp of bucket i.
func (g grid) at(i int) int64 { return (g.first + int64(i)*g.interval) * 1000 }

// index returns the bucket a unix-second timestamp falls in, and whether it is
// on the grid at all. A store may return a sample just outside the window, and
// a sketch's bucket start is compared against the same grid.
//
// The `sec < g.first` test is not redundant with a check on the quotient: Go
// divides towards zero, so one second *before* the first bucket gives a
// quotient of 0, not -1, and would be counted in bucket zero.
func (g grid) index(sec int64) (int, bool) {
	if sec < g.first {
		return 0, false
	}
	i := (sec - g.first) / g.interval
	if i >= int64(g.n) {
		return 0, false
	}
	return int(i), true
}

// endMs is the exclusive upper bound of the window in milliseconds.
func (g grid) endMs() int64 { return (g.first + int64(g.n)*g.interval) * 1000 }

// frame is what a node evaluates to: either a scalar, which broadcasts over
// whatever it meets, or a set of lines over the query's grid.
type frame struct {
	scalar   float64
	isScalar bool
	// metric labels the lines. It is a metric name when the frame came
	// straight from one query, and the node's canonical text once an
	// operation has combined two.
	metric string
	groups []*group
}

// group is one output line during evaluation: its group-by tags and its
// buckets.
type group struct {
	tags   map[string]string
	values []float64
}

// key identifies a group for a join: the group-by tags, sorted. Two groups
// with the same key are the same line on two sides of an operator.
func (g *group) key() string {
	keys := make([]string, 0, len(g.tags))
	for k := range g.tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(g.tags[k])
		b.WriteByte(0)
	}
	return b.String()
}

// lines turns a frame into the result's series, sorted so that a chart's
// legend and a golden file do not depend on map iteration order.
func (f frame) lines(n metricql.Node, g grid) []Series {
	if f.isScalar {
		// A bare number is still a line: a dashboard widget whose query is
		// `100` draws a threshold.
		pts := make([]Point, g.n)
		for i := range pts {
			pts[i] = Point{T: g.at(i), V: f.scalar}
		}
		return []Series{{Metric: n.String(), Tags: map[string]string{}, Points: pts}}
	}
	out := make([]Series, 0, len(f.groups))
	for _, grp := range f.groups {
		pts := make([]Point, g.n)
		for i := range pts {
			pts[i] = Point{T: g.at(i), V: grp.values[i]}
		}
		out = append(out, Series{Metric: f.metric, Tags: grp.tags, Points: pts})
	}
	slices.SortFunc(out, func(a, b Series) int { return strings.Compare(a.Scope(), b.Scope()) })
	return out
}

// node evaluates one AST node onto the grid.
func (e *Evaluator) node(ctx context.Context, n metricql.Node, g grid, st *state) (frame, error) {
	if err := ctx.Err(); err != nil {
		return frame{}, err
	}
	switch v := n.(type) {
	case *metricql.Number:
		return frame{scalar: v.Value, isScalar: true, metric: n.String()}, nil
	case *metricql.String:
		// The parser only accepts a string where a signature asks for one, so
		// reaching here means a function read its arguments wrongly.
		return frame{}, fmt.Errorf("the string %q is not a value", v.Value)
	case *metricql.Unary:
		f, err := e.node(ctx, v.X, g, st)
		if err != nil {
			return frame{}, err
		}
		return negate(f, n), nil
	case *metricql.Binary:
		return e.binary(ctx, v, g, st)
	case *metricql.Call:
		return e.call(ctx, v, g, st)
	case *metricql.Query:
		return e.query(ctx, v, g, st)
	}
	return frame{}, fmt.Errorf("cannot evaluate %T", n)
}
