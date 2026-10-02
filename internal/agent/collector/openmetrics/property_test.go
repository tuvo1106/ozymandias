package openmetrics

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/sketch"
)

// write renders families in Prometheus text (om=false) or OpenMetrics
// (om=true). Test-only: the agent never writes this format, and a printer in
// the package would be API with one caller.
func write(fams []Family, om bool) string {
	var b strings.Builder
	for _, f := range fams {
		if f.Help != "" {
			h := strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(f.Help)
			fmt.Fprintf(&b, "# HELP %s %s\n", f.Name, h)
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.Name, f.Type)
		for _, s := range f.Samples {
			b.WriteString(s.Name)
			if len(s.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range s.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(l.Value)
					fmt.Fprintf(&b, "%s=\"%s\"", l.Name, v)
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(s.Value, 'g', -1, 64))
			if s.HasTimestamp {
				if om {
					fmt.Fprintf(&b, " %s", strconv.FormatFloat(float64(s.TimestampMs)/1000, 'f', -1, 64))
				} else {
					fmt.Fprintf(&b, " %d", s.TimestampMs)
				}
			}
			b.WriteByte('\n')
		}
	}
	if om {
		b.WriteString("# EOF\n")
	}
	return b.String()
}

var (
	genMetricName = rapid.StringMatching(`[a-zA-Z_:][a-zA-Z0-9_:]{0,20}`)
	genLabelName  = rapid.StringMatching(`[a-zA-Z_][a-zA-Z0-9_]{0,12}`)
	// Label values are arbitrary text, escapes and all.
	genLabelValue = rapid.StringOf(rapid.SampledFrom([]rune("ab \"\\\n{},=#é\t")))
	genHelp       = rapid.StringOf(rapid.SampledFrom([]rune("ab \"\\\n#{}é")))
	genValue      = rapid.OneOf(
		rapid.Float64(),
		rapid.SampledFrom([]float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, 1, -1}),
	)
)

func genLabels(t *rapid.T) []Label {
	names := rapid.SliceOfNDistinct(genLabelName, 0, 4, func(s string) string { return s }).Draw(t, "labelNames")
	var ls []Label
	for _, n := range names {
		if n == "le" || n == "quantile" {
			continue
		}
		ls = append(ls, Label{n, genLabelValue.Draw(t, "value")})
	}
	return ls
}

// genFamilies draws gauge, counter and unknown families whose samples are
// named after the family, plus histograms with a full set of buckets.
func genFamilies(t *rapid.T) []Family {
	names := rapid.SliceOfNDistinct(genMetricName, 1, 5, func(s string) string { return s }).Draw(t, "names")
	var fams []Family
	taken := map[string]bool{}
	for _, n := range names {
		taken[n] = true
	}
	for _, name := range names {
		typ := rapid.SampledFrom([]Type{TypeGauge, TypeCounter, TypeUnknown, TypeHistogram}).Draw(t, "type")
		f := Family{Name: name, Type: typ, Help: genHelp.Draw(t, "help")}
		if typ == TypeHistogram {
			// Its sample names must not collide with another family's.
			if taken[name+"_bucket"] || taken[name+"_sum"] || taken[name+"_count"] {
				continue
			}
			ls := genLabels(t)
			bounds := rapid.SliceOfNDistinct(rapid.Float64Range(-1e6, 1e6), 0, 5, func(f float64) float64 { return f }).Draw(t, "bounds")
			slices.Sort(bounds)
			bounds = append(bounds, math.Inf(1))
			for _, ub := range bounds {
				labels := append(slices.Clone(ls), Label{"le", strconv.FormatFloat(ub, 'g', -1, 64)})
				f.Samples = append(f.Samples, Sample{Name: name + "_bucket", Labels: labels, Value: float64(rapid.IntRange(0, 1000).Draw(t, "n"))})
			}
			f.Samples = append(f.Samples,
				Sample{Name: name + "_sum", Labels: ls, Value: genValue.Draw(t, "sum")},
				Sample{Name: name + "_count", Labels: ls, Value: float64(rapid.IntRange(0, 1000).Draw(t, "count"))})
		} else {
			for range rapid.IntRange(1, 4).Draw(t, "samples") {
				s := Sample{Name: name, Labels: genLabels(t), Value: genValue.Draw(t, "v")}
				if rapid.Bool().Draw(t, "hasTS") {
					s.TimestampMs, s.HasTimestamp = rapid.Int64Range(-1e13, 1e13).Draw(t, "ts"), true
				}
				f.Samples = append(f.Samples, s)
			}
		}
		fams = append(fams, f)
	}
	return fams
}

// print → parse is the identity, in both formats: every value, label and
// escape a writer can produce comes back as it was — but a label with an
// empty value, which the formats define as no label at all.
func TestProperty_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		want := genFamilies(t)
		om := rapid.Bool().Draw(t, "openmetrics")
		body := write(want, om)
		for i := range want {
			for j := range want[i].Samples {
				want[i].Samples[j].Labels = slices.DeleteFunc(slices.Clone(want[i].Samples[j].Labels), func(l Label) bool { return l.Value == "" })
				if len(want[i].Samples[j].Labels) == 0 {
					want[i].Samples[j].Labels = nil
				}
			}
		}
		got, err := Parse(strings.NewReader(body), Options{})
		if err != nil {
			t.Fatalf("Parse: %v\n%s", err, body)
		}
		if !equalFamilies(got, want) {
			t.Fatalf("round trip differs\nbody:\n%s\ngot  %+v\nwant %+v", body, got, want)
		}
	})
}

// Counts are conserved: the per-bucket deltas are never negative and sum to
// the largest cumulative delta, and ToSketch puts exactly that many observations in the sketch.
func TestProperty_DeltasAndSketchConserveCounts(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		bounds := rapid.SliceOfNDistinct(rapid.Float64Range(-1e4, 1e4), 1, 8, func(f float64) float64 { return f }).Draw(t, "bounds")
		slices.Sort(bounds)
		bounds = append(bounds, math.Inf(1))
		cumulative := func(label string) []Bucket {
			bs := make([]Bucket, len(bounds))
			c := 0.0
			for i, ub := range bounds {
				c += float64(rapid.IntRange(0, 50).Draw(t, label))
				bs[i] = Bucket{ub, c}
			}
			return bs
		}
		prev, cur := cumulative("prev"), cumulative("cur")
		deltas, reset := BucketDeltas(prev, cur)
		// The largest cumulative delta: with a torn read (a lower bucket
		// ahead of a higher one) that exceeds the +Inf bucket's delta, and
		// the deltas must neither go negative nor lose it.
		want := 0.0
		for i := range cur {
			d := cur[i].Count
			if !reset {
				d -= prev[i].Count
			}
			want = math.Max(want, d)
		}
		var sum float64
		for i, d := range deltas {
			if d.Count < 0 || d.UpperBound != cur[i].UpperBound {
				t.Fatalf("delta %d = %+v", i, d)
			}
			sum += d.Count
		}
		if sum != want {
			t.Fatalf("deltas sum to %v, want %v (reset=%v)", sum, want, reset)
		}
		s := sketch.NewDefault()
		if err := ToSketch(deltas, s); err != nil {
			t.Fatalf("ToSketch: %v", err)
		}
		if math.Abs(s.Count()-sum) > 1e-9*math.Max(1, sum) {
			t.Fatalf("sketch holds %v, deltas %v", s.Count(), sum)
		}
	})
}
