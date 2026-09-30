package openmetrics

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/sketch"
)

// FuzzParse feeds arbitrary bodies to Parse (L4). A scrape target is
// untrusted input, so the properties are the ones that matter for that:
// never panic, stay within the limits, and when a body parses, everything
// downstream of it — grouping histograms, differencing their buckets,
// spreading them into a sketch — also runs without panicking.
func FuzzParse(f *testing.F) {
	pages, _ := filepath.Glob("testdata/*.prom")
	om, _ := filepath.Glob("testdata/*.om")
	for _, p := range append(pages, om...) {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	for _, s := range []string{
		"", "x 1", "x{a=\"\\\"\"} NaN 1", "# TYPE h histogram\nh_bucket{le=\"-1\"} 1\nh_bucket{le=\"+Inf\"} 2\n",
		"x 1 1.5\n# EOF\n", "x{a=\"b\",} +Inf # {t=\"1\"} 2", "# HELP x \\\\\\n\n", "\r\n",
	} {
		f.Add(s)
	}
	lim := Limits{MaxBytes: 1 << 16, MaxSamples: 500, MaxLabels: 8, MaxNameLen: 64, MaxLabelValueLen: 64}
	f.Fuzz(func(t *testing.T, body string) {
		fams, err := Parse(strings.NewReader(body), Options{Limits: lim})
		if err != nil {
			return
		}
		n := 0
		for _, fam := range fams {
			n += len(fam.Samples)
			for _, s := range fam.Samples {
				if len(s.Labels) > lim.MaxLabels {
					t.Fatalf("%d labels", len(s.Labels))
				}
				for _, l := range s.Labels {
					if len(l.Value) > lim.MaxLabelValueLen || len(l.Name) > lim.MaxNameLen {
						t.Fatalf("label %q=%q over the limit", l.Name, l.Value)
					}
				}
			}
			if fam.Type != TypeHistogram && fam.Type != TypeGaugeHistogram {
				continue
			}
			hs, err := fam.Histograms()
			if err != nil {
				continue
			}
			for _, h := range hs {
				deltas, _ := BucketDeltas(h.Buckets, h.Buckets)
				for _, d := range deltas {
					if d.Count != 0 && !math.IsNaN(d.Count) {
						t.Fatalf("a scrape minus itself has a non-zero delta: %+v", deltas)
					}
				}
				fromZero, _ := BucketDeltas([]Bucket{}, h.Buckets)
				_ = ToSketch(fromZero, sketch.NewDefault())
			}
		}
		if n > lim.MaxSamples {
			t.Fatalf("%d samples", n)
		}
	})
}

func BenchmarkParse_NodeExporter(b *testing.B) {
	body, err := os.ReadFile("testdata/node_exporter.prom")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(strings.NewReader(string(body)), Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
