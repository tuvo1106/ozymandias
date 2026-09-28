package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/sketch"
)

// MaxBinsPerRequest bounds the bins one distribution answer may carry.
//
// A sketch's bin count grows with the *ratio* between the largest and smallest
// value it has seen, not with how many observations there were: at the default
// relative accuracy, latencies spanning a millisecond to ten seconds occupy
// around 460 buckets. Multiply by 1500 output buckets and a few groups and the
// response is hundreds of megabytes of JSON describing a picture with more
// columns than a screen has pixels.
//
// 200000 is roughly a 4 MB response: enough for a heatmap far denser than one
// anybody can read, and small enough that a careless query is refused rather
// than served. The error says which two dials to turn, because neither is
// guessable from the outside.
const MaxBinsPerRequest = 200_000

// Bin is one bar of a distribution: the half-open value range (Lower, Upper]
// and how many observations fell in it.
//
// Resolved bounds rather than the sketch's bucket index, which is what the
// store holds. The index is meaningless without γ and the convention that γ^k
// is the bucket's *upper* bound, and shipping those two would make every client
// reimplement the mapping — including the sign rule, where a negative value's
// index is of its absolute value, so ascending index is descending value.
// Bounds are also stable under a future change of γ: a client that draws them
// keeps working, where one that drew indices would silently redraw the axis.
//
// Zero is its own bin, (0, 0], because log_γ 0 is undefined and a sketch counts
// zeros separately for exactly that reason.
type Bin struct {
	Lower, Upper float64
	Count        float64
}

// MarshalJSON writes [lower, upper, count], the same shape [Point] uses for a
// sample: an object per bin would triple the size of the field that dominates
// this response.
func (b Bin) MarshalJSON() ([]byte, error) {
	out := []byte{'['}
	for _, v := range []float64{b.Lower, b.Upper, b.Count} {
		if len(out) > 1 {
			out = append(out, ',')
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			// Unreachable from a stored sketch — the bounds are powers of γ and
			// the count is finite — but a null is a better answer than a body
			// that is not JSON, which is what a bare NaN would produce.
			out = append(out, "null"...)
			continue
		}
		out = strconv.AppendFloat(out, v, 'g', -1, 64)
	}
	return append(out, ']'), nil
}

// UnmarshalJSON reads [lower, upper, count].
func (b *Bin) UnmarshalJSON(data []byte) error {
	var triple [3]*float64
	if err := json.Unmarshal(data, &triple); err != nil {
		return fmt.Errorf("bin: want [lower, upper, count]: %s", data)
	}
	for i, v := range triple {
		if v == nil {
			return fmt.Errorf("bin: element %d is null: %s", i, data)
		}
	}
	b.Lower, b.Upper, b.Count = *triple[0], *triple[1], *triple[2]
	return nil
}

// DistBucket is one output bucket's merged distribution.
//
// Count, Sum, Min and Max are exact — a sketch carries them alongside the bins
// rather than estimating them from the bins — so a tooltip can show the real
// mean and the real maximum next to an approximate shape. Saying so matters:
// the bins are accurate to the store's relative accuracy and these four are
// not approximate at all.
type DistBucket struct {
	T     int64   `json:"t"`
	Count float64 `json:"count"`
	Sum   float64 `json:"sum"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Bins  []Bin   `json:"bins"`
}

// DistSeries is one group's distribution over time.
type DistSeries struct {
	Metric string            `json:"metric"`
	Tags   map[string]string `json:"tags"`
	Scope  string            `json:"scope"`
	// Buckets carries only the buckets something landed in. A heatmap wants a
	// gap where there was no traffic, and every bucket carries its own
	// timestamp, so an absent one is unambiguous — and 1500 empty objects to
	// say "nothing happened" is most of the response on a quiet metric.
	Buckets []DistBucket `json:"buckets"`
}

// Distribution is the answer to a `dist:` query.
type Distribution struct {
	From     int64 `json:"from"`
	To       int64 `json:"to"`
	Interval int64 `json:"interval"`
	// Gamma is the ratio between a bin's bounds, which is what the store's
	// relative accuracy comes to: α = (γ-1)/(γ+1). Reported because it is the
	// error bar on every bin in this response, and a client drawing a
	// distribution should be able to say how precise it is.
	Gamma  float64      `json:"gamma"`
	Series []DistSeries `json:"series"`
	// Bins is the total across every series, so a caller can see how close it
	// came to [MaxBinsPerRequest] before being refused.
	Bins     int      `json:"bins"`
	Warnings []string `json:"warnings"`
}

// Distribution answers `dist:metric{filter} by {keys}`: the merged sketch of
// every selected series, per bucket, as bins.
//
// It is its own entry point rather than a case inside [Evaluator.Eval] because
// the answer is not a number. Everything Eval returns is a series of floats
// that can be added, divided, compared to a threshold and drawn as a line; a
// distribution is none of those. Forcing it through the same return type would
// mean either a Result whose Series are empty and whose meaning lives in a
// side-channel, or a float that is secretly a handle — both of which are worse
// than a second method.
//
// The expression must be exactly one `dist:` query. Not a sum of two, not a
// ratio: merging is the only operation a distribution supports, and arithmetic
// on two distributions is a different (and much larger) question than this
// endpoint answers. The error says so rather than quietly using the first one.
func (e *Evaluator) Distribution(ctx context.Context, req Request) (Distribution, error) {
	q, err := distQuery(req.Expr)
	if err != nil {
		return Distribution{}, err
	}
	g, err := e.plan(req)
	if err != nil {
		return Distribution{}, err
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
	groups, err := e.sketchGroups(ctx, q, g, st, e.metricKind(q.Metric))
	if err != nil {
		return Distribution{}, err
	}

	out := Distribution{
		From:     req.From,
		To:       req.To,
		Interval: g.interval,
		Series:   make([]DistSeries, 0, len(groups)),
		Warnings: st.warnings,
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	for _, sg := range groups {
		s := DistSeries{
			Metric:  q.Metric,
			Tags:    sg.tags,
			Scope:   scopeOf(sg.tags),
			Buckets: make([]DistBucket, 0, g.n),
		}
		for i, sk := range sg.buckets {
			if sk == nil {
				continue
			}
			bins := binsOf(sk)
			out.Bins += len(bins)
			if out.Bins > MaxBinsPerRequest {
				return Distribution{}, badf(
					"this distribution needs more than %d bins; ask for a coarser interval, a narrower filter, or fewer groups",
					MaxBinsPerRequest)
			}
			s.Buckets = append(s.Buckets, DistBucket{
				T:     g.at(i),
				Count: sk.Count(),
				Sum:   sk.Sum(),
				Min:   sk.Min(),
				Max:   sk.Max(),
				Bins:  bins,
			})
		}
		// Gamma is a property of the stored sketches, not of the request, and
		// every sketch that merged agrees on it — sketchGroups refuses a group
		// whose sketches do not. Taken from the first bucket that exists, so it
		// describes the data rather than this build's default.
		if out.Gamma == 0 && len(s.Buckets) > 0 {
			for _, sk := range sg.buckets {
				if sk != nil {
					out.Gamma = sk.Gamma()
					break
				}
			}
		}
		out.Series = append(out.Series, s)
	}
	return out, nil
}

// distQuery checks that the expression is one `dist:` query and returns it.
func distQuery(n metricql.Node) (*metricql.Query, error) {
	q, ok := n.(*metricql.Query)
	if !ok {
		return nil, badf("this endpoint answers one `dist:` query; %s is an expression, and two distributions cannot be added or divided", n)
	}
	if q.Agg != metricql.Dist {
		return nil, badf("%s asks for a number, not a distribution: write `dist:%s{…}` here, or send this query to /api/v1/query", q, q.Metric)
	}
	return q, nil
}

// binsOf renders a merged sketch as value ranges, negatives first so the bins
// ascend by value the way an axis does.
//
// A negative bucket's index is of the absolute value, so its range is
// [-γ^k, -γ^(k-1)) and the list has to be walked backwards to come out
// ascending. Getting this the wrong way round draws a plausible histogram that
// is mirrored, which is why it is done here once rather than in each client.
func binsOf(s *sketch.Sketch) []Bin {
	gamma := s.Gamma()
	neg := s.NegativeBins()
	pos := s.PositiveBins()
	out := make([]Bin, 0, len(neg)+len(pos)+1)

	for i := len(neg) - 1; i >= 0; i-- {
		b := neg[i]
		out = append(out, Bin{
			Lower: -math.Pow(gamma, float64(b.Index)),
			Upper: -math.Pow(gamma, float64(b.Index-1)),
			Count: b.Count,
		})
	}
	if z := s.ZeroCount(); z > 0 {
		out = append(out, Bin{Lower: 0, Upper: 0, Count: z})
	}
	for _, b := range pos {
		out = append(out, Bin{
			Lower: math.Pow(gamma, float64(b.Index-1)),
			Upper: math.Pow(gamma, float64(b.Index)),
			Count: b.Count,
		})
	}
	return out
}
