package selfmetrics

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func TestReporter_CountersAreDeltasGaugesAreLevels(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("ozy.x.sent", "kind:a")
	g := reg.Gauge("ozy.x.depth")
	reg.GaugeFunc("ozy.x.nan", func() float64 { return math.NaN() })
	rep := NewReporter(reg, 10*time.Second, "host:h1")

	c.Add(5)
	g.Set(3)
	out := rep.Collect(time.Unix(1790000007, 0))
	if len(out) != 2 {
		t.Fatalf("got %d series (NaN gauge must be skipped): %+v", len(out), out)
	}
	byName := map[string]wire.Series{}
	for _, s := range out {
		byName[s.Metric] = s
		if err := wire.ValidateSeries(&s, wire.DecodeOptions{Now: time.Unix(1790000007, 0)}); err != nil {
			t.Errorf("%s is not valid on the wire: %v", s.Metric, err)
		}
	}
	sent := byName["ozy.x.sent"]
	if sent.Type != wire.KindCount || sent.Interval != 10 || sent.Points[0] != (wire.Point{Timestamp: 1790000000, Value: 5}) {
		t.Fatalf("counter = %+v", sent)
	}
	if strings.Join(sent.Tags, ",") != "host:h1,kind:a" {
		t.Fatalf("tags = %v", sent.Tags)
	}
	if d := byName["ozy.x.depth"]; d.Type != wire.KindGauge || d.Points[0].Value != 3 {
		t.Fatalf("gauge = %+v", d)
	}

	c.Add(2)
	out = rep.Collect(time.Unix(1790000017, 0))
	for _, s := range out {
		if s.Metric == "ozy.x.sent" && s.Points[0].Value != 2 {
			t.Fatalf("second delta = %v, want 2", s.Points[0].Value)
		}
	}
}

func TestNewReporter_DefaultsTheInterval(t *testing.T) {
	if NewReporter(NewRegistry(), 0).interval != 10 {
		t.Fatal("interval default")
	}
}

// A shutdown flush calls Collect a second time inside the same interval. The
// store is last-write-wins, so emitting the delta-since-the-first-call would
// replace a full interval's counts with whatever happened in the last instant.
func TestReporter_DoesNotReemitABucketItAlreadyReported(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("ozy.test.events")
	r := NewReporter(reg, 10*time.Second, "host:h")

	c.Add(5000)
	first := r.Collect(time.Unix(100, 0))
	if len(first) != 1 || first[0].Points[0].Value != 5000 {
		t.Fatalf("first collect = %+v", first)
	}

	// Three more events, then shutdown five seconds into the same bucket.
	c.Add(3)
	if got := r.Collect(time.Unix(105, 0)); got != nil {
		t.Fatalf("second collect in the same bucket returned %+v, want nil", got)
	}

	// The next bucket reports everything since the last *reported* point.
	next := r.Collect(time.Unix(110, 0))
	if len(next) != 1 || next[0].Points[0].Value != 3 {
		t.Fatalf("next bucket = %+v, want the 3 events since the last report", next)
	}
}
