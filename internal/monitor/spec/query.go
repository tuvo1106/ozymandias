package spec

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

// Bounds on a monitor's window.
const (
	// MinWindow is the shortest window: one agent flush is ten seconds, so a
	// window shorter than a minute holds a handful of points at most and a
	// monitor over it alerts on noise.
	MinWindow = time.Minute
	// MaxWindow bounds what one evaluation reads. A monitor runs every thirty
	// seconds forever; a window of a week would re-read a week of data twice a
	// minute to answer a question that changes by thirty seconds' worth.
	MaxWindow = 24 * time.Hour
)

// Op is a comparison, spelled as written.
type Op string

// The comparisons.
const (
	OpGT Op = ">"
	OpGE Op = ">="
	OpLT Op = "<"
	OpLE Op = "<="
	OpEQ Op = "=="
	OpNE Op = "!="
)

// Compare reports whether v op t.
func (o Op) Compare(v, t float64) bool {
	switch o {
	case OpGT:
		return v > t
	case OpGE:
		return v >= t
	case OpLT:
		return v < t
	case OpLE:
		return v <= t
	case OpEQ:
		return v == t
	case OpNE:
		return v != t
	}
	return false
}

// Above reports whether the op alerts on high values. It decides which side of
// a threshold its recovery threshold must sit on.
func (o Op) Above() bool { return o == OpGT || o == OpGE }

// Below reports whether the op alerts on low values.
func (o Op) Below() bool { return o == OpLT || o == OpLE }

// TimeAgg reduces a group's points over the window to the one value compared
// against thresholds.
type TimeAgg string

// The time aggregations.
const (
	AggAvg  TimeAgg = "avg"
	AggSum  TimeAgg = "sum"
	AggMin  TimeAgg = "min"
	AggMax  TimeAgg = "max"
	AggLast TimeAgg = "last"
	// AggPctNonNull is the percentage of the window's buckets that hold a
	// value. It is how "this heartbeat is flaky" is written, as opposed to
	// "this heartbeat stopped" (no data).
	AggPctNonNull TimeAgg = "pct_nonnull"
)

var timeAggs = []TimeAgg{AggAvg, AggSum, AggMin, AggMax, AggLast, AggPctNonNull}

// Query is a parsed monitor query: exactly one of Metric and Log is set.
type Query struct {
	Metric *MetricQuery
	Log    *LogQuery
	// Window is how far back from the evaluation point the query reads.
	Window time.Duration
	// Op and Threshold are the comparison written in the query. The threshold
	// must equal the definition's thresholds.critical.
	Op        Op
	Threshold float64
}

// MetricQuery is `time_agg(last_N):<metricql>`.
type MetricQuery struct {
	Agg TimeAgg
	// Expr is the parsed metricql. A `by {…}` in it makes the monitor a
	// multi-alert, with state per group.
	Expr metricql.Node
}

// LogQuery is `logs("…").rollup("count")[.by("key")].last("N")`.
type LogQuery struct {
	// Filter is the logql text, unquoted; Expr is it parsed.
	Filter string
	Expr   logql.Node
	// By is the one field counts are split by; "" for one count. One key and
	// not several because the log store's histogram groups by one field, and
	// a monitor that silently grouped by the first of three would be wrong in
	// a way nobody could see.
	By string
}

// tail matches the comparison at the end of a query. Anchored at the end
// because the number is the one thing that cannot appear inside a metricql
// tag value unquoted *and* end the string: reading from the right means a '>'
// inside `{route:/a>b}` is never mistaken for the comparison.
var tail = regexp.MustCompile(`\s*(>=|<=|==|!=|>|<)\s*([-+]?[0-9]*\.?[0-9]+(?:[eE][-+]?[0-9]+)?)\s*$`)

// metricHead matches `avg(last_5m):`.
var metricHead = regexp.MustCompile(`^\s*([a-z_]+)\(last_([0-9]+)([mh])\):`)

// ParseQuery parses a monitor query.
//
// The grammar is in docs/query-language.md#monitors. It is two languages
// wrapped in a little of a third: the metricql or logql inside is parsed by
// its own parser, so a monitor accepts exactly what a dashboard does and
// reports the same errors.
func ParseQuery(q string) (Query, error) {
	if len(q) > 4096 {
		return Query{}, fmt.Errorf("query is %d bytes; the limit is 4096", len(q))
	}
	m := tail.FindStringSubmatchIndex(q)
	if m == nil {
		return Query{}, fmt.Errorf("a monitor query ends with a comparison against a number, e.g. `> 20`")
	}
	op := Op(q[m[2]:m[3]])
	threshold, err := strconv.ParseFloat(q[m[4]:m[5]], 64)
	if err != nil || math.IsInf(threshold, 0) || math.IsNaN(threshold) {
		return Query{}, fmt.Errorf("threshold %q is not a finite number", q[m[4]:m[5]])
	}
	body := strings.TrimSpace(q[:m[0]])
	out := Query{Op: op, Threshold: threshold}
	if strings.HasPrefix(body, "logs(") {
		lq, window, err := parseLogQuery(body)
		if err != nil {
			return Query{}, err
		}
		out.Log, out.Window = lq, window
		return out, nil
	}
	h := metricHead.FindStringSubmatch(body)
	if h == nil {
		return Query{}, fmt.Errorf("a metric monitor starts with a time aggregation over a window, e.g. `avg(last_5m):`; a log monitor with `logs(\"…\")`")
	}
	agg := TimeAgg(h[1])
	if !validAgg(agg) {
		return Query{}, fmt.Errorf("time aggregation %q: want one of %s", h[1], aggList())
	}
	window, err := window(h[2], h[3])
	if err != nil {
		return Query{}, err
	}
	rest := body[len(h[0]):]
	if strings.TrimSpace(rest) == "" {
		return Query{}, fmt.Errorf("no metric query after %q", h[0])
	}
	expr, err := metricql.Parse(rest)
	if err != nil {
		return Query{}, fmt.Errorf("metric query: %w", err)
	}
	if err := checkRollups(expr, window); err != nil {
		return Query{}, err
	}
	out.Metric, out.Window = &MetricQuery{Agg: agg, Expr: expr}, window
	return out, nil
}

// checkRollups requires an explicit `.rollup(_, seconds)` to be a step the
// evaluation grid can use: one that divides the window (or the window would
// hold a partial bucket, or none: a 3600 s step over a 5m window reads
// nothing and never alerts) and divides a day (the store floors buckets to
// multiples of the step since the epoch, and the window is aligned the same
// way only when the step divides a day). Every rollup in an arithmetic
// expression must agree, since the evaluation has one grid.
func checkRollups(expr metricql.Node, window time.Duration) error {
	var steps []int64
	metricql.Walk(expr, func(n metricql.Node) {
		if q, ok := n.(*metricql.Query); ok {
			for _, m := range q.Modifiers {
				if m.Kind == metricql.ModRollup && m.Seconds > 0 {
					steps = append(steps, m.Seconds)
				}
			}
		}
	})
	for _, st := range steps {
		if st != steps[0] {
			return fmt.Errorf("rollup intervals %ds and %ds differ; a monitor evaluates on one grid", steps[0], st)
		}
		w := int64(window / time.Second)
		if st > w || w%st != 0 || 86400%st != 0 {
			return fmt.Errorf("rollup interval %ds must divide both the %v window and a day (86400s)", st, window)
		}
	}
	return nil
}

func validAgg(a TimeAgg) bool {
	for _, x := range timeAggs {
		if x == a {
			return true
		}
	}
	return false
}

func aggList() string {
	s := make([]string, len(timeAggs))
	for i, a := range timeAggs {
		s[i] = string(a)
	}
	return strings.Join(s, ", ")
}

// window reads `5` `m` as a duration and bounds it.
func window(n, unit string) (time.Duration, error) {
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v > 1_000_000 {
		return 0, fmt.Errorf("window %s%s is not a duration", n, unit)
	}
	d := time.Duration(v) * time.Minute
	if unit == "h" {
		d = time.Duration(v) * time.Hour
	}
	if d < MinWindow || d > MaxWindow {
		return 0, fmt.Errorf("window %s%s: want between %v and %v", n, unit, MinWindow, MaxWindow)
	}
	return d, nil
}

// parseLogQuery reads `logs("…").rollup("count")[.by("key")].last("N")`.
//
// A hand-written cursor rather than a regular expression, because the first
// argument is a quoted string that may itself contain `").rollup("`, and a
// cursor that unquotes with strconv knows where the string ends.
func parseLogQuery(body string) (*LogQuery, time.Duration, error) {
	c := &cursor{s: body}
	if !c.lit("logs(") {
		return nil, 0, c.errf("want logs(\"…\")")
	}
	filter, err := c.quoted()
	if err != nil {
		return nil, 0, err
	}
	if !c.lit(")") {
		return nil, 0, c.errf("want ) after the log query")
	}
	if !c.lit(".rollup(") {
		return nil, 0, c.errf(`want .rollup("count")`)
	}
	method, err := c.quoted()
	if err != nil {
		return nil, 0, err
	}
	if method != "count" {
		return nil, 0, fmt.Errorf(`rollup %q: a log monitor counts, so the only rollup is "count"`, method)
	}
	if !c.lit(")") {
		return nil, 0, c.errf("want ) after the rollup")
	}
	lq := &LogQuery{Filter: filter}
	if c.lit(".by(") {
		if lq.By, err = c.quoted(); err != nil {
			return nil, 0, err
		}
		if c.peek(",") {
			return nil, 0, c.errf("a log monitor groups by one field")
		}
		if !c.lit(")") {
			return nil, 0, c.errf("want ) after the group-by field")
		}
		if err := validLogGroup(lq.By); err != nil {
			return nil, 0, err
		}
	}
	if !c.lit(".last(") {
		return nil, 0, c.errf(`want .last("5m")`)
	}
	w, err := c.quoted()
	if err != nil {
		return nil, 0, err
	}
	if !c.lit(")") {
		return nil, 0, c.errf("want ) after the window")
	}
	if strings.TrimSpace(c.s[c.i:]) != "" {
		return nil, 0, c.errf("unexpected text")
	}
	wm := regexp.MustCompile(`^([0-9]+)([mh])$`).FindStringSubmatch(w)
	if wm == nil {
		return nil, 0, fmt.Errorf("window %q: want minutes or hours, e.g. \"5m\"", w)
	}
	window, err := window(wm[1], wm[2])
	if err != nil {
		return nil, 0, err
	}
	if lq.Expr, err = logql.Parse(filter); err != nil {
		return nil, 0, fmt.Errorf("log query: %w", err)
	}
	return lq, window, nil
}

// validLogGroup accepts what the log store can group by: a reserved label or
// an @attribute.
func validLogGroup(by string) error {
	if strings.HasPrefix(by, "@") && len(by) > 1 {
		return nil
	}
	for _, k := range logql.ReservedKeys {
		if by == k {
			return nil
		}
	}
	return fmt.Errorf("cannot group logs by %q: want one of %s or an @attribute", by, strings.Join(logql.ReservedKeys, ", "))
}

type cursor struct {
	s string
	i int
}

func (c *cursor) skip() {
	for c.i < len(c.s) && (c.s[c.i] == ' ' || c.s[c.i] == '\t' || c.s[c.i] == '\n') {
		c.i++
	}
}

func (c *cursor) lit(want string) bool {
	c.skip()
	if strings.HasPrefix(c.s[c.i:], want) {
		c.i += len(want)
		return true
	}
	return false
}

func (c *cursor) peek(want string) bool {
	c.skip()
	return strings.HasPrefix(c.s[c.i:], want)
}

// quoted reads one double-quoted Go string literal.
func (c *cursor) quoted() (string, error) {
	c.skip()
	if c.i >= len(c.s) || c.s[c.i] != '"' {
		return "", c.errf("want a quoted string")
	}
	for j := c.i + 1; j < len(c.s); j++ {
		switch c.s[j] {
		case '\\':
			j++
		case '"':
			v, err := strconv.Unquote(c.s[c.i : j+1])
			if err != nil {
				return "", c.errf("bad string: %v", err)
			}
			c.i = j + 1
			return v, nil
		}
	}
	return "", c.errf("unterminated string")
}

func (c *cursor) errf(format string, a ...any) error {
	return fmt.Errorf("col %d: %s", c.i+1, fmt.Sprintf(format, a...))
}

// String prints a query back in canonical form: the inner metricql in its own
// canonical spelling, so two monitors that mean the same thing print the same.
func (q Query) String() string {
	cmp := fmt.Sprintf(" %s %s", q.Op, strconv.FormatFloat(q.Threshold, 'g', -1, 64))
	if q.Log != nil {
		s := fmt.Sprintf("logs(%s).rollup(\"count\")", strconv.Quote(q.Log.Filter))
		if q.Log.By != "" {
			s += fmt.Sprintf(".by(%s)", strconv.Quote(q.Log.By))
		}
		return s + fmt.Sprintf(".last(%q)", durText(q.Window)) + cmp
	}
	return fmt.Sprintf("%s(last_%s):%s", q.Metric.Agg, durText(q.Window), q.Metric.Expr) + cmp
}

func durText(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// Grouped reports whether the query is a multi-alert: state per group.
func (q Query) Grouped() bool {
	if q.Log != nil {
		return q.Log.By != ""
	}
	grouped := false
	metricql.Walk(q.Metric.Expr, func(n metricql.Node) {
		if mq, ok := n.(*metricql.Query); ok && len(mq.By) > 0 {
			grouped = true
		}
	})
	return grouped
}
