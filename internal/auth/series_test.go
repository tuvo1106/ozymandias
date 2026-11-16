package auth

import (
	"fmt"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func trackerPrincipal(t *testing.T, perMetric, total int) (*Principal, *testutil.FakeClock, *selfmetrics.Registry) {
	t.Helper()
	clk := testutil.NewFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	reg := selfmetrics.NewRegistry()
	p := newPrincipal(1, "k", Limits{MaxSeriesPerMetric: perMetric, MaxActiveSeries: total}, clk)
	p.rej = NewRejections(reg)
	return p, clk, reg
}

func rejected(reg *selfmetrics.Registry, reason string) float64 {
	for _, m := range reg.Snapshot() {
		if m.Name == "ozy.intake.rejected" && len(m.Tags) == 1 && m.Tags[0] == "reason:"+reason {
			return m.Value
		}
	}
	return -1
}

func TestAdmitSeries_PerMetricLimitRefusesTheNextNewSeriesOnly(t *testing.T) {
	p, _, reg := trackerPrincipal(t, 3, 0)
	for i := 0; i < 3; i++ {
		if ok, why := p.AdmitSeries("m", []string{fmt.Sprintf("user:%d", i)}); !ok {
			t.Fatalf("series %d refused under the limit: %s", i, why)
		}
	}
	if ok, why := p.AdmitSeries("m", []string{"user:3"}); ok || why == "" {
		t.Fatalf("a fourth series was admitted (ok=%v why=%q)", ok, why)
	}
	// A series already counted is not new: a sender that keeps sending its three
	// series must never be refused for them.
	for i := 0; i < 3; i++ {
		if ok, _ := p.AdmitSeries("m", []string{fmt.Sprintf("user:%d", i)}); !ok {
			t.Fatalf("an active series %d was refused", i)
		}
	}
	// Another metric has its own room.
	if ok, _ := p.AdmitSeries("other", []string{"user:0"}); !ok {
		t.Fatal("a different metric was refused for another metric's series")
	}
	if got := rejected(reg, ReasonSeriesPerMetric); got != 1 {
		t.Errorf("series_per_metric rejections = %v, want 1", got)
	}
}

func TestAdmitSeries_TotalLimitCountsEveryMetric(t *testing.T) {
	p, _, reg := trackerPrincipal(t, 0, 4)
	for i := 0; i < 4; i++ {
		if ok, _ := p.AdmitSeries(fmt.Sprintf("m%d", i), nil); !ok {
			t.Fatalf("metric %d refused under the total", i)
		}
	}
	if ok, _ := p.AdmitSeries("m4", nil); ok {
		t.Fatal("a fifth series was admitted over a total of 4")
	}
	if got := rejected(reg, ReasonSeriesTotal); got != 1 {
		t.Errorf("series_total rejections = %v, want 1", got)
	}
}

func TestAdmitSeries_AnIdleSeriesAgesOutAndFreesItsSlot(t *testing.T) {
	p, clk, _ := trackerPrincipal(t, 2, 0)
	p.AdmitSeries("m", []string{"a"})
	p.AdmitSeries("m", []string{"b"})
	if ok, _ := p.AdmitSeries("m", []string{"c"}); ok {
		t.Fatal("over the limit")
	}
	clk.Advance(activeWindow / 2)
	p.AdmitSeries("m", []string{"a"}) // a is still being sent; b is not
	clk.Advance(activeWindow/2 + activeWindow/4)
	if ok, _ := p.AdmitSeries("m", []string{"c"}); !ok {
		t.Fatal("b was idle for longer than the window and its slot was not freed")
	}
	if ok, _ := p.AdmitSeries("m", []string{"b"}); ok {
		t.Fatal("a and c are active, so b is a third series")
	}
	if ok, _ := p.AdmitSeries("m", []string{"a"}); !ok {
		t.Fatal("a was refreshed and must have stayed")
	}
}

// What bounds the memory: whatever a hostile sender names, the tracker never holds more
// than its limit, and a limit on one metric leaves the total capped too.
func TestAdmitSeries_HostileInputNeverGrowsTheTrackerPastItsLimit(t *testing.T) {
	p, _, _ := trackerPrincipal(t, 0, 1000)
	for i := 0; i < 100_000; i++ {
		p.AdmitSeries(fmt.Sprintf("m%d", i%7000), []string{fmt.Sprintf("id:%d", i)})
	}
	if n := len(p.series.seen); n > 1000 {
		t.Fatalf("the tracker holds %d series under a limit of 1000", n)
	}
	q, _, _ := trackerPrincipal(t, 5, 0) // a per-metric limit alone still has a ceiling
	if q.series.total != hardSeriesCap {
		t.Fatalf("total = %d, want the hard cap %d", q.series.total, hardSeriesCap)
	}
}

func TestAdmitSeries_NoLimitsMeansNoTrackerAndANilPrincipalAdmitsAll(t *testing.T) {
	p, _, _ := trackerPrincipal(t, 0, 0)
	if p.series != nil {
		t.Fatal("a key with no series limit has a tracker (memory for nothing)")
	}
	if ok, _ := p.AdmitSeries("m", nil); !ok {
		t.Fatal("refused with no limit")
	}
	var none *Principal
	if ok, _ := none.AdmitSeries("m", nil); !ok {
		t.Fatal("auth off must admit everything")
	}
}

func TestLimits_SeriesFieldsTakeDefaultsAndANegativeMeansUnlimited(t *testing.T) {
	d := Limits{MaxSeriesPerMetric: 100, MaxActiveSeries: 1000}
	got := Limits{}.withDefaults(d)
	if got.MaxSeriesPerMetric != 100 || got.MaxActiveSeries != 1000 {
		t.Fatalf("defaults not applied: %+v", got)
	}
	got = Limits{MaxSeriesPerMetric: -1, MaxActiveSeries: 7}.withDefaults(d)
	if got.MaxSeriesPerMetric != 0 || got.MaxActiveSeries != 7 {
		t.Fatalf("a key's own values: %+v", got)
	}
}
