package naive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

var ctx = context.Background()

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func ss(metric string, tags []string, samples ...tsdb.Sample) tsdb.SeriesSamples {
	return tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(metric, tags), Samples: samples}
}

func sm(t int64, v float64) tsdb.Sample { return tsdb.Sample{T: t, V: v} }

// dump renders a SeriesSet as "key=t:v,t:v; key=...".
func dump(t *testing.T, set tsdb.SeriesSet) string {
	t.Helper()
	var parts []string
	for set.Next() {
		var pts []string
		it := set.Iterator()
		for it.Next() {
			pts = append(pts, fmt.Sprintf("%d:%g", it.At().T, it.At().V))
		}
		parts = append(parts, set.Series().Key()+"="+strings.Join(pts, ","))
	}
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	_ = set.Close()
	return strings.Join(parts, "; ")
}

func mustAppend(t *testing.T, s *Store, batch ...tsdb.SeriesSamples) tsdb.AppendResult {
	t.Helper()
	res, err := s.Append(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStore_AppendAndSelect(t *testing.T) {
	s, _ := open(t)
	res := mustAppend(t, s,
		ss("http.count", []string{"route:/a", "env:dev"}, sm(1000, 1), sm(2000, 2)),
		ss("http.count", []string{"route:/b", "env:dev"}, sm(1000, 5)),
		ss("other", nil, sm(1000, 9)),
	)
	if res.Series != 3 || res.Samples != 4 || len(res.Rejected) != 0 {
		t.Fatalf("result %+v", res)
	}
	set, err := s.Select(ctx, tsdb.Selector{Metric: "http.count"}, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	want := "http.count|env:dev,route:/a=1000:1,2000:2; http.count|env:dev,route:/b=1000:5"
	if got := dump(t, set); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestStore_SelectBoundsAreInclusiveAndEmptySeriesOmitted(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s,
		ss("m", []string{"a:1"}, sm(1000, 1), sm(2000, 2), sm(3000, 3)),
		ss("m", []string{"a:2"}, sm(9000, 9)),
	)
	set, _ := s.Select(ctx, tsdb.Selector{Metric: "m"}, 2000, 3000)
	if got := dump(t, set); got != "m|a:1=2000:2,3000:3" {
		t.Fatalf("got %s", got)
	}
}

func TestStore_DuplicateTimestampIsLastWriteWins(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s, ss("m", nil, sm(1000, 1)))
	mustAppend(t, s, ss("m", nil, sm(1000, 7)))
	set, _ := s.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 5000)
	if got := dump(t, set); got != "m|=1000:7" {
		t.Fatalf("got %s", got)
	}
	if st := s.Stats(); st.Series != 1 || st.Samples != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestStore_Matchers(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s,
		ss("m", []string{"env:dev", "route:/api/comics"}, sm(1, 1)),
		ss("m", []string{"env:prod", "route:/api/users"}, sm(1, 2)),
		ss("m", []string{"env:dev", "route:/web"}, sm(1, 3)),
		ss("m", []string{"route:/api/x"}, sm(1, 4)),
	)
	for _, tc := range []struct {
		m    []tsdb.Matcher
		want []float64
	}{
		{[]tsdb.Matcher{{Key: "env", Value: "dev", Type: tsdb.Equal}}, []float64{1, 3}},
		{[]tsdb.Matcher{{Key: "env", Value: "dev", Type: tsdb.NotEqual}}, []float64{2, 4}},
		{[]tsdb.Matcher{{Key: "route", Value: "/api/*", Type: tsdb.Wildcard}}, []float64{1, 2, 4}},
		{[]tsdb.Matcher{{Key: "route", Value: "/api/*", Type: tsdb.NotWildcard}}, []float64{3}},
		{[]tsdb.Matcher{{Key: "env", Value: "dev"}, {Key: "route", Value: "/api/*", Type: tsdb.Wildcard}}, []float64{1}},
		{[]tsdb.Matcher{{Key: "nope", Value: "x"}}, nil},
	} {
		set, err := s.Select(ctx, tsdb.Selector{Metric: "m", Matchers: tc.m}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []float64
		for set.Next() {
			it := set.Iterator()
			for it.Next() {
				got = append(got, it.At().V)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.m, got, tc.want)
		}
	}
}

func TestStore_RejectsBadSeriesButKeepsTheBatch(t *testing.T) {
	s, _ := open(t)
	res := mustAppend(t, s,
		tsdb.SeriesSamples{Series: tsdb.SeriesRef{Metric: "m", Tags: []tsdb.Tag{{Key: "b"}, {Key: "a"}}}, Samples: []tsdb.Sample{sm(1, 1)}},
		ss("m", nil, sm(1, math.NaN())),
		ss("m", nil, sm(1, math.Inf(1))),
		ss("ok", nil, sm(1, 1)),
	)
	if res.Series != 1 || len(res.Rejected) != 3 {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Rejected[0].Reason, "sorted") || !strings.Contains(res.Rejected[1].Reason, "non-finite") {
		t.Fatalf("reasons %+v", res.Rejected)
	}
}

func TestStore_MetadataLookups(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s,
		ss("http.request.count", []string{"route:/a", "env:dev", "canary"}, sm(1, 1)),
		ss("http.request.count", []string{"route:/b", "env:prod"}, sm(1, 1)),
		ss("http.request.duration", []string{"route:/a"}, sm(1, 1)),
		ss("jobs", []string{"queue:q"}, sm(1, 1)),
		ss("h_x", nil, sm(1, 1)),
		ss("hax", nil, sm(1, 1)),
	)
	names, err := s.MetricNames(ctx, "http.", 0)
	if err != nil || strings.Join(names, ",") != "http.request.count,http.request.duration" {
		t.Errorf("MetricNames(http.) = %v, %v", names, err)
	}
	if names, _ := s.MetricNames(ctx, "", 2); strings.Join(names, ",") != "h_x,hax" {
		t.Errorf("MetricNames limit = %v", names)
	}
	if names, _ := s.MetricNames(ctx, "h_", 0); strings.Join(names, ",") != "h_x" {
		t.Errorf("'_' in a prefix must be literal: %v", names)
	}
	keys, _ := s.TagKeys(ctx, "http.request.count")
	if strings.Join(keys, ",") != "canary,env,route" {
		t.Errorf("TagKeys = %v", keys)
	}
	vals, _ := s.TagValues(ctx, "http.request.count", "route", 0)
	if strings.Join(vals, ",") != "/a,/b" {
		t.Errorf("TagValues = %v", vals)
	}
	if vals, _ := s.TagValues(ctx, "http.request.count", "canary", 0); len(vals) != 0 {
		t.Errorf("bare tag values = %v, want none", vals)
	}
	if vals, _ := s.TagValues(ctx, "http.request.count", "env", 1); strings.Join(vals, ",") != "dev" {
		t.Errorf("TagValues limit = %v", vals)
	}
	if keys, _ := s.TagKeys(ctx, "none"); keys == nil || len(keys) != 0 {
		t.Errorf("unknown metric keys = %#v, want empty non-nil", keys)
	}
}

func TestStore_ReopenKeepsDataAndIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, ss("m", []string{"a:1"}, sm(1, 1)))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	mustAppend(t, s, ss("m", []string{"a:1"}, sm(2, 2)))
	if st := s.Stats(); st.Series != 1 || st.Samples != 2 {
		t.Fatalf("stats after reopen %+v (series duplicated?)", st)
	}
}

func TestStore_SameNewSeriesTwiceInOneBatch(t *testing.T) {
	s, _ := open(t)
	res := mustAppend(t, s, ss("m", []string{"a:1"}, sm(1, 1)), ss("m", []string{"a:1"}, sm(2, 2)))
	if res.Series != 2 || s.Stats().Series != 1 || s.Stats().Samples != 2 {
		t.Fatalf("res %+v stats %+v", res, s.Stats())
	}
}

func TestStore_ConcurrentAppendsAndSelects(t *testing.T) {
	s, _ := open(t)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 50 {
				if _, err := s.Append(ctx, []tsdb.SeriesSamples{ss("m", []string{fmt.Sprintf("w:%d", w)}, sm(int64(i), 1))}); err != nil {
					t.Error(err)
					return
				}
			}
		})
		wg.Go(func() {
			for range 20 {
				if _, err := s.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 100); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if st := s.Stats(); st.Series != 4 || st.Samples != 200 {
		t.Fatalf("stats %+v", st)
	}
}

func TestOpen_Errors(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing-dir", "x.db")); err == nil {
		t.Fatal("opened a store in a directory that doesn't exist")
	}
}

func TestStore_ClosedStoreErrors(t *testing.T) {
	s, _ := open(t)
	_ = s.Close()
	if _, err := s.Append(ctx, []tsdb.SeriesSamples{ss("m", nil, sm(1, 1))}); err == nil {
		t.Error("Append on a closed store")
	}
	if _, err := s.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 1); err == nil {
		t.Error("Select on a closed store")
	}
	if _, err := s.MetricNames(ctx, "", 0); err == nil {
		t.Error("MetricNames on a closed store")
	}
	if st := s.Stats(); st != (tsdb.StoreStats{}) {
		t.Error("Stats on a closed store")
	}
}

func TestOpen_NotADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not sqlite ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opened a file that isn't a database")
	}
}

func TestStore_CorruptTagsAreAnErrorNotAGuess(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s, ss("m", []string{"a:1"}, sm(1, 1)))
	if _, err := s.db.Exec(`UPDATE series SET tags_json = 'nope'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 10); err == nil || !strings.Contains(err.Error(), "corrupt tags") {
		t.Fatalf("err = %v", err)
	}
}

func TestStore_CancelledContext(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s, ss("m", []string{"a:1"}, sm(1, 1)))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Append(cctx, []tsdb.SeriesSamples{ss("n", nil, sm(1, 1))}); err == nil {
		t.Error("Append with a cancelled context")
	}
	if _, err := s.Select(cctx, tsdb.Selector{Metric: "m"}, 0, 10); err == nil {
		t.Error("Select with a cancelled context")
	}
	if _, err := s.TagKeys(cctx, "m"); err == nil {
		t.Error("TagKeys with a cancelled context")
	}
	// Nothing half-written: the store still has exactly the first series.
	if st := s.Stats(); st.Series != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// A failed batch must not leave ids cached for series that were rolled back.
func TestStore_FailedBatchLeavesNoStaleIDs(t *testing.T) {
	s, _ := open(t)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_big BEFORE INSERT ON samples WHEN NEW.v > 100 BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, []tsdb.SeriesSamples{ss("m", []string{"a:1"}, sm(1, 1)), ss("m", []string{"a:2"}, sm(1, 500))}); err == nil {
		t.Fatal("trigger did not fire")
	}
	if len(s.ids) != 0 {
		t.Fatalf("ids cached after rollback: %v", s.ids)
	}
	mustAppend(t, s, ss("m", []string{"a:1"}, sm(1, 1)))
	set, _ := s.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 10)
	if got := dump(t, set); got != "m|a:1=1:1" {
		t.Fatalf("got %s", got)
	}
}

func TestQuery_ScanErrorStopsAndReports(t *testing.T) {
	s, _ := open(t)
	mustAppend(t, s, ss("m", nil, sm(1, 1)))
	boom := fmt.Errorf("boom")
	_, err := query(ctx, s.db, func(*sql.Rows) (int, error) { return 0, boom }, `SELECT id FROM series`)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

// --- benchmarks (L12) ------------------------------------------------------

// BenchmarkStore_Append measures the intake's write path: one flush from one
// agent, which is a batch of many series with one sample each. The cost that
// matters is per series, not per sample, because every series in the batch
// costs a lookup and an insert inside a single transaction.
func BenchmarkStore_Append(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			s := openBench(b)
			batch := make([]tsdb.SeriesSamples, n)
			for i := range batch {
				batch[i] = ss("http.request.count", []string{"route:/r" + strconv.Itoa(i)}, sm(0, 1))
			}
			b.ReportAllocs()
			b.ResetTimer()
			ts := int64(0)
			for b.Loop() {
				ts += 10_000 // a fresh 10s bucket, so no row is overwritten
				for i := range batch {
					batch[i].Samples[0].T = ts
				}
				if _, err := s.Append(ctx, batch); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n), "series/op")
		})
	}
}

// BenchmarkStore_Select measures a one-hour query over a single series, the
// shape the UI issues on every refresh.
func BenchmarkStore_Select(b *testing.B) {
	s := openBench(b)
	const points = 360 // one hour of 10s buckets
	batch := []tsdb.SeriesSamples{ss("http.request.count", []string{"route:/api"}, make([]tsdb.Sample, points)...)}
	for i := range batch[0].Samples {
		batch[0].Samples[i] = sm(int64(i)*10_000, float64(i))
	}
	if _, err := s.Append(ctx, batch); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		set, err := s.Select(ctx, tsdb.Selector{Metric: "http.request.count"}, 0, points*10_000)
		if err != nil {
			b.Fatal(err)
		}
		got := 0
		for set.Next() {
			it := set.Iterator()
			for it.Next() {
				got++
			}
		}
		if err := set.Err(); err != nil {
			b.Fatal(err)
		}
		if err := set.Close(); err != nil {
			b.Fatal(err)
		}
		if got != points {
			b.Fatalf("got %d points", got)
		}
	}
}

func openBench(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "metrics.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}
