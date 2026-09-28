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
			// A null bound means "past what a float64 can say", and it is
			// reachable: γ^k overflows above index ≈35490 at the default
			// accuracy, and a bin index is only bounded by ±2^31 — sketch.Add
			// clamps to that, and the wire format accepts anything inside it, so
			// a client POSTing a large index produces one. The previous version
			// of this comment called it unreachable, which was wrong twice over:
			// it is reachable, and [Bin.UnmarshalJSON] then refused the bin this
			// method had just emitted.
			out = append(out, "null"...)
			continue
		}
		out = strconv.AppendFloat(out, v, 'g', -1, 64)
	}
	return append(out, ']'), nil
}

// UnmarshalJSON reads [lower, upper, count].
//
// A null *bound* is accepted and means unbounded on that side — which is what
// [Bin.MarshalJSON] emits when γ^k overflows a float64. A null *count* is not:
// a bin whose count is unknown is not a bin, and accepting one would put a NaN
// into whatever the client sums.
func (b *Bin) UnmarshalJSON(data []byte) error {
	var triple [3]*float64
	if err := json.Unmarshal(data, &triple); err != nil {
		return fmt.Errorf("bin: want [lower, upper, count]: %s", data)
	}
	if triple[2] == nil {
		return fmt.Errorf("bin: the count is null: %s", data)
	}
	b.Lower, b.Upper = math.Inf(-1), math.Inf(1)
	if triple[0] != nil {
		b.Lower = *triple[0]
	}
	if triple[1] != nil {
		b.Upper = *triple[1]
	}
	b.Count = *triple[2]
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
	T int64 `json:"t"`
	// Gamma is the ratio between this bucket's bin bounds, which is what the
	// relative accuracy of the sketches that merged into it comes to:
	// α = (γ-1)/(γ+1).
	//
	// Per bucket, not per response. Every sketch merging into *one* bucket
	// agrees on γ — sketchGroups refuses a group whose sketches do not — but two
	// buckets need not, and neither need two groups: reconfigure one host's
	// accuracy and `dist:lat{*} by {host}` legitimately returns several. A single
	// top-level γ was therefore a number that described the first bucket and was
	// quietly applied to the rest. A client wanting one error bar for the axis
	// can take the largest.
	Gamma float64   `json:"gamma"`
	Count jsonFloat `json:"count"`
	Sum   jsonFloat `json:"sum"`
	Min   jsonFloat `json:"min"`
	Max   jsonFloat `json:"max"`
	Bins  []Bin     `json:"bins"`
}

// jsonFloat is a float64 that leaves as null when it is not finite.
//
// It exists because of a bug this endpoint shipped with for exactly one review
// cycle: an empty sketch reports min as +Inf and max as -Inf — its sentinels —
// and the intake stores count-0 buckets deliberately. encoding/json refuses a
// non-finite float, so one such bucket anywhere in the window made json.Marshal
// of the *whole* response fail, and the reader got a 500 instead of a heatmap.
//
// A count-0 bucket is now skipped, which is the real fix; this is the guard
// behind it. "It cannot happen because of what is upstream" is what the previous
// comment on [Bin.MarshalJSON] said, and it was wrong too.
type jsonFloat float64

// MarshalJSON writes the number, or null when it is not finite.
func (f jsonFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, v, 'g', -1, 64), nil
}

// UnmarshalJSON reads a number, or null as NaN — so a value that left as null
// comes back as something a client can test rather than as a silent zero.
func (f *jsonFloat) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = jsonFloat(math.NaN())
		return nil
	}
	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*f = jsonFloat(v)
	return nil
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
	From     int64        `json:"from"`
	To       int64        `json:"to"`
	Interval int64        `json:"interval"`
	Series   []DistSeries `json:"series"`
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
			// A bucket with no observations is an empty bucket, whatever the
			// reason it exists: the store keeps count-0 sketches, and one of
			// them here used to make the whole response a 500 because an empty
			// sketch's min is +Inf and encoding/json refuses that. There is
			// nothing to draw either way — see [DistSeries.Buckets].
			if sk == nil || sk.Count() == 0 {
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
				Gamma: sk.Gamma(),
				Count: jsonFloat(sk.Count()),
				Sum:   jsonFloat(sk.Sum()),
				Min:   jsonFloat(sk.Min()),
				Max:   jsonFloat(sk.Max()),
				Bins:  bins,
			})
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
	// Modifiers are refused rather than ignored.
	//
	// Every one of them describes something done to a *line*: `.as_rate()`
	// divides by the interval, `.fill()` invents the buckets nothing reported,
	// `.rollup()` names how samples reduce into a bucket. A distribution has no
	// line to do any of that to — merging is the only reduction it has — so this
	// path would have applied none of them. It did exactly that for one review
	// cycle: `dist:lat{*}.as_rate()` was accepted, ignored, and not even
	// warned about, which is the thing internal/query/metricql/eval/modifier.go
	// argues against in its own first paragraph.
	//
	// The bucket width is the one thing somebody might really have meant, and it
	// has a spelling that works: the `interval` parameter.
	if len(q.Modifiers) > 0 {
		return nil, badf(
			"%s: a distribution has no line to modify, so `.%s` would be ignored — merging is its only reduction, and the bucket width is the `interval` parameter",
			q, q.Modifiers[0].Kind)
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
