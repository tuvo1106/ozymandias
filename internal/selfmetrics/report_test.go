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

// A process stopped before its first whole bucket reports nothing rather
// than the bucket its predecessor reported (ADR-0029); what it counted is
// reported at the first bucket after its start, if it lives that long.
func TestReporter_NotBeforeTheFirstWholeBucket(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("ozy.test.events")
	r := NewReporter(reg, 10*time.Second, "host:h")
	r.NotBefore(time.Unix(100, 0)) // started on a boundary: 100 may be the predecessor's

	c.Add(2)
	for _, now := range []int64{100, 105, 109} {
		if got := r.Collect(time.Unix(now, 0)); got != nil {
			t.Fatalf("collect at %d = %+v, want nothing before 110", now, got)
		}
	}
	got := r.Collect(time.Unix(110, 0))
	if len(got) != 1 || got[0].Points[0] != (wire.Point{Timestamp: 110, Value: 2}) {
		t.Fatalf("collect at 110 = %+v, want the 2 events at 110", got)
	}
}

func TestFirstBucket(t *testing.T) {
	for _, tc := range []struct{ started, want int64 }{
		{100, 110}, // a boundary: that bucket may be the predecessor's
		{101, 110},
		{109, 110},
		{-5, 0}, // before the epoch, still floored down
	} {
		if got := FirstBucket(time.Unix(tc.started, 0), 10); got != tc.want {
			t.Errorf("FirstBucket(%d) = %d, want %d", tc.started, got, tc.want)
		}
	}
}

// values reads what a Collect reported for name, by its tags without host.
func values(out []wire.Series, name string) []float64 {
	var v []float64
	for _, s := range out {
		if s.Metric == name {
			v = append(v, s.Points[0].Value)
		}
	}
	return v
}

// A released instrument reports its last value once and is then gone, from
// the registry and from the Reporter's memory: a new one of the same
// identity counts from zero rather than from the old total.
func TestReporter_ReleasedInstrumentsReportOnceThenGo(t *testing.T) {
	reg := NewRegistry()
	rep := NewReporter(reg, 10*time.Second)
	c := reg.Counter("ozy.x.runs", "collector:a")
	g := reg.Gauge("ozy.x.ms", "collector:a")
	c.Add(3)
	g.Set(9)
	rep.Collect(time.Unix(1790000000, 0))
	c.Add(2)
	reg.Release("ozy.x.runs", "collector:a")
	reg.Release("ozy.x.ms", "collector:a")
	out := rep.Collect(time.Unix(1790000010, 0))
	if v := values(out, "ozy.x.runs"); len(v) != 1 || v[0] != 2 {
		t.Fatalf("the last report of a released counter = %v, want [2]", v)
	}
	if v := values(out, "ozy.x.ms"); len(v) != 1 || v[0] != 9 {
		t.Fatalf("the last report of a released gauge = %v, want [9]", v)
	}
	if out := rep.Collect(time.Unix(1790000020, 0)); len(out) != 0 || len(reg.Snapshot()) != 0 || len(rep.prev) != 0 {
		t.Fatalf("released instruments were kept: reported %+v, registry %+v, prev %v", out, reg.Snapshot(), rep.prev)
	}
	reg.Counter("ozy.x.runs", "collector:a").Add(1)
	if v := values(rep.Collect(time.Unix(1790000030, 0)), "ozy.x.runs"); len(v) != 1 || v[0] != 1 {
		t.Fatalf("a new counter of a dropped identity reported %v, want [1]", v)
	}
}

// An instrument two holders share stays while either holds it, and one
// taken back between its release and its report keeps its total: dropping
// prev then would report the whole total again as new.
func TestReporter_ReleaseCountsHolders(t *testing.T) {
	reg := NewRegistry()
	rep := NewReporter(reg, 10*time.Second)
	a := reg.Counter("ozy.x.runs")
	reg.Counter("ozy.x.runs")
	a.Add(4)
	reg.Release("ozy.x.runs")
	rep.Collect(time.Unix(1790000000, 0))
	a.Add(1)
	if v := values(rep.Collect(time.Unix(1790000010, 0)), "ozy.x.runs"); len(v) != 1 || v[0] != 1 {
		t.Fatalf("a counter one of two holders released = %v, want [1]", v)
	}

	reg.Release("ozy.x.runs")
	snap := reg.Snapshot()
	if len(snap) != 1 || !snap[0].released {
		t.Fatalf("the last holder's release did not mark it: %+v", snap)
	}
	b := reg.Counter("ozy.x.runs") // taken back before its last report
	b.Add(2)
	if v := values(rep.Collect(time.Unix(1790000020, 0)), "ozy.x.runs"); len(v) != 1 || v[0] != 2 {
		t.Fatalf("a counter taken back = %v, want [2]", v)
	}
	b.Add(1)
	if v := values(rep.Collect(time.Unix(1790000030, 0)), "ozy.x.runs"); len(v) != 1 || v[0] != 1 {
		t.Fatalf("a counter taken back then reported %v, want [1], not its total again", v)
	}
}

// Only the Reporter drops: a snapshot served over HTTP must not take a
// released counter's last increments before the Reporter sends them.
func TestRegistry_SnapshotAloneDropsNothing(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("ozy.x.runs").Add(5)
	reg.Release("ozy.x.runs")
	reg.Snapshot()
	if v := values(NewReporter(reg, 10*time.Second).Collect(time.Unix(1790000000, 0)), "ozy.x.runs"); len(v) != 1 || v[0] != 5 {
		t.Fatalf("after a plain snapshot the Reporter saw %v, want [5]", v)
	}
}

// Reporter.Collect for a bucket it has already reported drops nothing: the
// released instrument waits for the next report that is actually sent.
func TestReporter_ASkippedCollectDropsNothing(t *testing.T) {
	reg := NewRegistry()
	rep := NewReporter(reg, 10*time.Second)
	c := reg.Counter("ozy.x.runs")
	rep.Collect(time.Unix(1790000000, 0))
	c.Add(3)
	reg.Release("ozy.x.runs")
	if out := rep.Collect(time.Unix(1790000005, 0)); len(out) != 0 {
		t.Fatalf("a second Collect in one bucket reported %+v", out)
	}
	if v := values(rep.Collect(time.Unix(1790000010, 0)), "ozy.x.runs"); len(v) != 1 || v[0] != 3 {
		t.Fatalf("the released counter's last report = %v, want [3]", v)
	}
}

// Taken back after the Reporter's snapshot but before its drop, an
// instrument stays: drop re-checks the holders rather than trusting the
// snapshot, and says so, so the Reporter keeps its memory of the counter.
func TestRegistry_DropKeepsAnInstrumentTakenBack(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("ozy.x.runs").Add(5)
	reg.Release("ozy.x.runs")
	snap := reg.Snapshot()
	reg.Counter("ozy.x.runs")
	if reg.drop(snap[0]) {
		t.Fatal("drop removed an instrument that was taken back")
	}
	if s := reg.Snapshot(); len(s) != 1 || s[0].Value != 5 || s[0].released {
		t.Fatalf("after drop: %+v", s)
	}
}
