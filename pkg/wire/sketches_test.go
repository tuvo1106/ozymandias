package wire

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// goodSketch is a small, valid sketch: 7 observations in three buckets.
func goodSketch() Sketch {
	return Sketch{
		Gamma: 1.0202020202020203,
		Count: 7, Sum: 803.1, Min: 2.1, Max: 96,
		Zeros:   0,
		Bins:    []SketchBin{{Index: 35, Count: 2}, {Index: 36, Count: 1}, {Index: 41, Count: 4}},
		NegBins: []SketchBin{},
	}
}

func goodSeries() SketchSeries {
	return SketchSeries{
		Metric:   "http.request.duration",
		Tags:     []string{"service:checkout", "route:/api/items"},
		Interval: 10,
		Points:   []SketchPoint{{Timestamp: 1790000000, Sketch: goodSketch()}},
	}
}

func decodeOpts() DecodeOptions { return DecodeOptions{Now: time.Unix(1790000005, 0)} }

// The documented example in wire-protocol.md §D must decode. A normative doc
// whose own example is rejected is worse than no doc.
func TestDecodeSketches_TheDocumentedExample(t *testing.T) {
	body := []byte(`{
	  "sketches": [
	    {
	      "metric": "http.request.duration",
	      "tags": ["env:dev", "host:host-1", "route:/api/items", "service:checkout"],
	      "interval": 10,
	      "points": [
	        {
	          "ts": 1790000000,
	          "sketch": {
	            "gamma": 1.0202020202,
	            "count": 57, "sum": 803.1, "min": 2.1, "max": 96.0,
	            "zero_count": 0,
	            "bins": [[35, 4], [36, 9], [41, 44]],
	            "neg_bins": []
	          }
	        }
	      ]
	    }
	  ]
	}`)
	valid, rejects, err := DecodeSketches(body, decodeOpts())
	if err != nil {
		t.Fatalf("DecodeSketches: %v", err)
	}
	if len(rejects) != 0 {
		t.Fatalf("rejected: %v", rejects)
	}
	if len(valid) != 1 || len(valid[0].Points) != 1 {
		t.Fatalf("got %d series: %+v", len(valid), valid)
	}
	sk := valid[0].Points[0].Sketch
	if sk.Count != 57 || len(sk.Bins) != 3 || sk.Bins[2] != (SketchBin{Index: 41, Count: 44}) {
		t.Errorf("sketch: %+v", sk)
	}
	if got := valid[0].Tags; len(got) != 4 || got[0] != "env:dev" {
		t.Errorf("tags not canonical: %v", got)
	}
}

// A sketch round-trips through JSON unchanged. This is the property the
// golden file pins for the *bytes*; this pins the values.
func TestSketch_JSONRoundTrip(t *testing.T) {
	in := goodSeries()
	// Canonical tags going in, so any difference coming out is the encoding's
	// and not the decoder's documented re-ordering.
	in.Tags = CanonicalTags(in.Tags)
	in.Points[0].Sketch.NegBins = []SketchBin{{Index: -3, Count: 1.5}}
	in.Points[0].Sketch.Zeros = 2
	in.Points[0].Sketch.Count = 10.5

	body, err := json.Marshal(SketchesPayload{Sketches: []SketchSeries{in}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	valid, rejects, err := DecodeSketches(body, decodeOpts())
	if err != nil || len(rejects) != 0 {
		t.Fatalf("DecodeSketches: %v %v", err, rejects)
	}
	got, err := json.Marshal(SketchesPayload{Sketches: valid})
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round trip changed the payload:\n got %s\nwant %s", got, body)
	}
}

// Every way one series can be wrong, and the one thing that must stay true of
// all of them: the series is rejected, the body is not.
func TestDecodeSketches_RejectsOneSeriesNotTheBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		fix  func(*SketchSeries)
	}{
		{"bad metric name", "invalid metric name", func(s *SketchSeries) { s.Metric = "1nope" }},
		{"no interval", "positive interval", func(s *SketchSeries) { s.Interval = 0 }},
		{"bad tag", "invalid tag", func(s *SketchSeries) { s.Tags = []string{"no colon or anything!"} }},
		{"no points", "no points", func(s *SketchSeries) { s.Points = nil }},
		{"timestamp at zero", "not a positive unix time", func(s *SketchSeries) { s.Points[0].Timestamp = 0 }},
		{"far future", "in the future", func(s *SketchSeries) { s.Points[0].Timestamp = 1790000000 + 3600 }},
		{"gamma of one", "gamma", func(s *SketchSeries) { s.Points[0].Sketch.Gamma = 1 }},
		{"negative count", "count", func(s *SketchSeries) { s.Points[0].Sketch.Count = -1 }},
		{"min above max", "min", func(s *SketchSeries) { s.Points[0].Sketch.Min = 1e9 }},
		{"negative zero count", "zero_count", func(s *SketchSeries) { s.Points[0].Sketch.Zeros = -1 }},
		{"empty bucket", "must be finite and positive", func(s *SketchSeries) {
			s.Points[0].Sketch.Bins[1].Count = 0
		}},
		{"buckets out of order", "must ascend", func(s *SketchSeries) {
			s.Points[0].Sketch.Bins[1].Index = 3
		}},
		{"duplicate bucket", "must ascend", func(s *SketchSeries) {
			s.Points[0].Sketch.Bins[1].Index = 35
		}},
		{"buckets disagree with count", "count says", func(s *SketchSeries) {
			s.Points[0].Sketch.Count = 999
			s.Points[0].Sketch.Max = 1e9
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSeries()
			tc.fix(&s)
			body, err := json.Marshal(SketchesPayload{Sketches: []SketchSeries{s}})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			valid, rejects, err := DecodeSketches(body, decodeOpts())
			if err != nil {
				t.Fatalf("whole body rejected, want a per-series rejection: %v", err)
			}
			if len(valid) != 0 {
				t.Fatalf("accepted %+v", valid)
			}
			if len(rejects) != 1 {
				t.Fatalf("rejections: %+v", rejects)
			}
			if !strings.Contains(rejects[0].Reason, tc.want) {
				t.Errorf("reason %q does not mention %q", rejects[0].Reason, tc.want)
			}
		})
	}
}

// A body that is not a sketch payload at all is the only 400.
func TestDecodeSketches_BodyLevelErrors(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not json", `{`, "not a sketch payload"},
		{"wrong shape", `[]`, "not a sketch payload"},
		{"no sketches key", `{"series":[]}`, `no "sketches" array`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := DecodeSketches([]byte(tc.body), decodeOpts())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// The per-request and per-series caps exist so a decoder cannot be made to
// allocate without bound, which is only true if they are enforced before the
// allocation rather than after.
func TestDecodeSketches_Limits(t *testing.T) {
	t.Run("too many series is a body error", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"sketches":[`)
		for i := 0; i <= MaxSketchesPerRequest; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{}`)
		}
		b.WriteString(`]}`)
		if _, _, err := DecodeSketches([]byte(b.String()), decodeOpts()); err == nil ||
			!strings.Contains(err.Error(), "the limit is") {
			t.Errorf("got %v, want a limit error", err)
		}
	})

	t.Run("too many buckets is a series rejection", func(t *testing.T) {
		s := goodSeries()
		bins := make([]SketchBin, MaxBinsPerSketch+1)
		for i := range bins {
			bins[i] = SketchBin{Index: i, Count: 1}
		}
		s.Points[0].Sketch.Bins = bins
		s.Points[0].Sketch.Count = float64(len(bins))
		body, err := json.Marshal(SketchesPayload{Sketches: []SketchSeries{s}})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		_, rejects, err := DecodeSketches(body, decodeOpts())
		if err != nil || len(rejects) != 1 || !strings.Contains(rejects[0].Reason, "the limit is") {
			t.Fatalf("err=%v rejects=%+v", err, rejects)
		}
	})

	t.Run("too many points is a series rejection", func(t *testing.T) {
		s := goodSeries()
		s.Points = make([]SketchPoint, MaxPointsPerSketchSeries+1)
		for i := range s.Points {
			s.Points[i] = SketchPoint{Timestamp: 1000 + int64(i), Sketch: goodSketch()}
		}
		body, err := json.Marshal(SketchesPayload{Sketches: []SketchSeries{s}})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		_, rejects, err := DecodeSketches(body, decodeOpts())
		if err != nil || len(rejects) != 1 || !strings.Contains(rejects[0].Reason, "the limit is") {
			t.Fatalf("err=%v rejects=%+v", err, rejects)
		}
	})
}

// A bin is a pair, and everything that is not one is an error rather than a
// zero value quietly standing in for a bucket.
func TestSketchBin_JSON(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"not an array", `5`, "want [index, count]"},
		{"one element", `[1]`, "got 1 elements"},
		{"three elements", `[1,2,3]`, "got 3 elements"},
		{"fractional index", `[1.5, 2]`, "want an integer"},
		{"index past int32", `[2147483648, 2]`, "outside"},
		{"unparseable count", `[1, "x"]`, "count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b SketchBin
			err := json.Unmarshal([]byte(tc.in), &b)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	// A count with no JSON form must fail loudly rather than write bytes no
	// decoder can read back.
	if _, err := json.Marshal(SketchBin{Index: 1, Count: math.Inf(1)}); err == nil {
		t.Error("marshalling an infinite count succeeded")
	}
}

func FuzzDecodeSketches(f *testing.F) {
	f.Add([]byte(`{"sketches":[{"metric":"a","interval":10,"tags":["k:v"],"points":[{"ts":100,"sketch":{"gamma":1.02,"count":1,"sum":1,"min":1,"max":1,"zero_count":0,"bins":[[1,1]],"neg_bins":[]}}]}]}`))
	f.Add([]byte(`{"sketches":[{"metric":"a","interval":10,"points":[{"ts":100,"sketch":{"gamma":1e400}}]}]}`))
	f.Add([]byte(`{"sketches":[{"metric":"a","interval":10,"points":[{"ts":100,"sketch":{"gamma":1.02,"count":0,"bins":[]}}]}]}`))
	f.Add([]byte(`{"sketches":[]}`))
	f.Add([]byte(`[]`))
	now := time.Unix(200, 0)
	f.Fuzz(func(t *testing.T, body []byte) {
		valid, _, err := DecodeSketches(body, DecodeOptions{Now: now})
		if err != nil {
			return
		}
		for i := range valid {
			if err := ValidateSketchSeries(&valid[i], DecodeOptions{Now: now}); err != nil {
				t.Fatalf("accepted series fails validation: %v", err)
			}
			if _, err := json.Marshal(valid[i]); err != nil {
				t.Fatalf("accepted series does not re-encode: %v", err)
			}
		}
	})
}
