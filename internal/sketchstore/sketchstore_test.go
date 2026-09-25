package sketchstore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

func open(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = filepath.Join(t.TempDir(), "sketches")
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func ref(metric string, tags ...string) tsdb.SeriesRef {
	return tsdb.NewSeriesRef(metric, tags)
}

// sketchOf builds a sketch over values, failing the test rather than the
// caller's line of sight.
func sketchOf(t *testing.T, values ...float64) *sketch.Sketch {
	t.Helper()
	s := sketch.NewDefault()
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatalf("Add(%v): %v", v, err)
		}
	}
	return s
}

func mustAppend(t *testing.T, s *Store, entries ...Entry) AppendResult {
	t.Helper()
	res, err := s.Append(context.Background(), entries)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected: %+v", res.Rejected)
	}
	return res
}

func TestStore_WriteReadRoundTrip(t *testing.T) {
	s := open(t, Options{})
	r := ref("http.request.duration", "service:checkout")
	orig := sketchOf(t, 1, 2, 3, 50, 900)

	mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: 1_700_000_000_000, Sketch: orig}}})

	got, err := s.Read(context.Background(), r, 1_700_000_000_000, 1_700_000_000_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if got[0].TimeMs != 1_700_000_000_000 {
		t.Errorf("ts: got %d, want %d", got[0].TimeMs, 1_700_000_000_000)
	}
	assertSameSketch(t, got[0].Sketch, orig)
}

func assertSameSketch(t *testing.T, got, want *sketch.Sketch) {
	t.Helper()
	if got.Count() != want.Count() || got.Sum() != want.Sum() ||
		got.Min() != want.Min() || got.Max() != want.Max() ||
		got.ZeroCount() != want.ZeroCount() || got.Gamma() != want.Gamma() {
		t.Fatalf("aggregates differ:\n got %+v\nwant %+v", got.ToWire(), want.ToWire())
	}
	for _, q := range []float64{0, 0.5, 0.95, 1} {
		a, err1 := want.Quantile(q)
		b, err2 := got.Quantile(q)
		if err1 != nil || err2 != nil {
			t.Fatalf("Quantile(%v): %v %v", q, err1, err2)
		}
		if a != b {
			t.Errorf("q=%v: got %v, want %v", q, b, a)
		}
	}
}

// Both bounds inclusive, ascending by time, and nothing outside the window —
// the same contract tsdb.MetricStore.Select has, because the query layer
// reads the two together and cannot afford them to differ.
func TestStore_ReadIsInclusiveAndOrdered(t *testing.T) {
	s := open(t, Options{})
	r := ref("d", "k:v")
	var points []Point
	for i := range 5 {
		points = append(points, Point{TimeMs: int64(1000+i*10) * 1000, Sketch: sketchOf(t, float64(i+1))})
	}
	mustAppend(t, s, Entry{Series: r, Points: points})

	got, err := s.Read(context.Background(), r, 1010*1000, 1030*1000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var times []int64
	for _, p := range got {
		times = append(times, p.TimeMs/1000)
	}
	want := []int64{1010, 1020, 1030}
	if len(times) != len(want) {
		t.Fatalf("times: got %v, want %v", times, want)
	}
	for i := range want {
		if times[i] != want[i] {
			t.Fatalf("times: got %v, want %v", times, want)
		}
	}
}

// A redelivered payload must not double-count. The agent retries until ozyd
// acknowledges, so the same bucket arrives twice whenever an acknowledgement
// is lost, and a store that merged would turn every retry into inflation.
func TestStore_AppendingTheSameBucketTwiceReplaces(t *testing.T) {
	s := open(t, Options{})
	r := ref("d", "k:v")
	first := sketchOf(t, 1, 2, 3)
	mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: 5000, Sketch: first}}})
	mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: 5000, Sketch: first}}})

	got, err := s.Read(context.Background(), r, 0, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1 — a retry created a second bucket", len(got))
	}
	if got[0].Sketch.Count() != 3 {
		t.Errorf("count: got %v, want 3 — a retry was merged instead of replacing", got[0].Sketch.Count())
	}
}

// The ids survive a restart because they are computed from the series, not
// handed out — that is the whole reason for hashing rather than assigning.
func TestStore_SurvivesAReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sketches")
	r := ref("d", "k:v")
	orig := sketchOf(t, 7, 8, 9)

	first, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, first, Entry{Series: r, Points: []Point{{TimeMs: 9000, Sketch: orig}}})
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing twice must be a no-op, got: %v", err)
	}

	second, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close() //nolint:errcheck // the assertions below are the point

	if got := second.Stats().Series; got != 1 {
		t.Errorf("series after reopen: got %d, want 1", got)
	}
	got, err := second.Read(context.Background(), r, 0, 20_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d points after reopen, want 1", len(got))
	}
	assertSameSketch(t, got[0].Sketch, orig)
}

// Two series that hash alike would otherwise share a percentile. The store
// keeps the canonical key beside the id so it can tell, and says which series
// it refused.
func TestStore_RejectsAnIDCollision(t *testing.T) {
	s := open(t, Options{})
	held := ref("d", "k:v")
	mustAppend(t, s, Entry{Series: held, Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}}})

	// Forge the collision: claim the held id for a different series by
	// writing that series' id into the map. This is what a genuine FNV
	// collision would look like from the store's side.
	other := ref("other.metric", "k:v")
	s.mu.Lock()
	s.ids[SeriesID(other)] = held.Key()
	s.mu.Unlock()

	res, err := s.Append(context.Background(), []Entry{{Series: other, Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 2)}}}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(res.Rejected) != 1 {
		t.Fatalf("rejected: %+v", res.Rejected)
	}
	if !strings.Contains(res.Rejected[0].Reason, ErrIDCollision.Error()) ||
		!strings.Contains(res.Rejected[0].Reason, held.Key()) {
		t.Errorf("reason %q should name the collision and the series already holding the id",
			res.Rejected[0].Reason)
	}
	if res.Series != 0 {
		t.Errorf("stored %d series, want 0", res.Series)
	}
	// And the held series' data is untouched.
	got, err := s.Read(context.Background(), held, 0, 10_000)
	if err != nil || len(got) != 1 || got[0].Sketch.Count() != 1 {
		t.Fatalf("the held series changed: %v %+v", err, got)
	}
}

// A series whose id belongs to someone else reads as empty rather than
// handing back another metric's sketches.
func TestStore_ReadingAColludingIDReturnsNothing(t *testing.T) {
	s := open(t, Options{})
	held := ref("d", "k:v")
	mustAppend(t, s, Entry{Series: held, Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}}})

	other := ref("other.metric", "k:v")
	s.mu.Lock()
	s.ids[SeriesID(other)] = held.Key()
	s.mu.Unlock()

	got, err := s.Read(context.Background(), other, 0, 10_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d points for a series that owns none", len(got))
	}
}

func TestStore_RejectsWhatItCannotStore(t *testing.T) {
	s := open(t, Options{})
	for _, tc := range []struct {
		name  string
		entry Entry
	}{
		{"non-canonical tags", Entry{
			Series: tsdb.SeriesRef{Metric: "d", Tags: []tsdb.Tag{{Key: "b"}, {Key: "a"}}},
			Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}},
		}},
		{"no metric", Entry{
			Series: tsdb.SeriesRef{},
			Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}},
		}},
		{"nil sketch", Entry{
			Series: ref("d", "k:v"),
			Points: []Point{{TimeMs: 1000, Sketch: nil}},
		}},
		{"timestamp before the epoch", Entry{
			Series: ref("d", "k:v2"),
			Points: []Point{{TimeMs: -1, Sketch: sketchOf(t, 1)}},
		}},
		{"timestamp past 2106", Entry{
			Series: ref("d", "k:v3"),
			Points: []Point{{TimeMs: (MaxTimestamp + 1) * 1000, Sketch: sketchOf(t, 1)}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.Append(context.Background(), []Entry{tc.entry})
			if err != nil {
				t.Fatalf("Append returned a store error, want a rejection: %v", err)
			}
			if len(res.Rejected) != 1 {
				t.Fatalf("rejections: %+v", res.Rejected)
			}
		})
	}
}

// One bad series in a batch must not lose the good ones: the agent sends
// hundreds per flush and cannot retry a partial success.
func TestStore_OneBadSeriesDoesNotFailTheBatch(t *testing.T) {
	s := open(t, Options{})
	good := ref("d", "k:good")
	res, err := s.Append(context.Background(), []Entry{
		{Series: good, Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}}},
		{Series: ref("d", "k:bad"), Points: []Point{{TimeMs: 1000, Sketch: nil}}},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if res.Series != 1 || res.Points != 1 || len(res.Rejected) != 1 {
		t.Fatalf("result: %+v", res)
	}
	got, err := s.Read(context.Background(), good, 0, 10_000)
	if err != nil || len(got) != 1 {
		t.Fatalf("the good series was lost: %v %+v", err, got)
	}
}

// Retention deletes by age and forgets a series it empties, because the id
// list is what the next sweep walks — a store that kept every series it ever
// saw would sweep a growing list of nothing forever.
func TestStore_TruncateDropsOldPointsAndEmptySeries(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(10_000, 0))
	s := open(t, Options{Clock: clk, Retention: time.Hour})

	old, fresh := ref("d", "age:old"), ref("d", "age:fresh")
	mustAppend(t, s,
		Entry{Series: old, Points: []Point{{TimeMs: 1_000_000, Sketch: sketchOf(t, 1)}}},
		Entry{Series: fresh, Points: []Point{
			{TimeMs: 1_000_000, Sketch: sketchOf(t, 1)},
			{TimeMs: 9_000_000, Sketch: sketchOf(t, 2)},
		}},
	)
	if got := s.Stats().Series; got != 2 {
		t.Fatalf("series: got %d, want 2", got)
	}

	// Cutoff at 5000s: the old series loses its only point, the fresh one
	// keeps the newer of its two.
	dropped, err := s.Truncate(context.Background(), 5_000_000)
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped %d series, want 1", dropped)
	}
	if got := s.Stats().Series; got != 1 {
		t.Errorf("series after truncate: got %d, want 1", got)
	}
	if got, err := s.Read(context.Background(), old, 0, MaxTimestamp*1000); err != nil || len(got) != 0 {
		t.Errorf("the old series still has %d points (%v)", len(got), err)
	}
	got, err := s.Read(context.Background(), fresh, 0, MaxTimestamp*1000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].TimeMs != 9_000_000 {
		t.Errorf("fresh series: got %+v, want one point at 9000000", got)
	}
}

// Sweep is Truncate with the cutoff taken from the clock, and a negative
// retention means the sweep does nothing at all.
func TestStore_Sweep(t *testing.T) {
	// Clock at 10000s with an hour of retention puts the cutoff at 6400s.
	clk := testutil.NewFakeClock(time.Unix(10_000, 0))
	s := open(t, Options{Clock: clk, Retention: time.Hour})
	old, fresh := ref("d", "age:old"), ref("d", "age:fresh")
	mustAppend(t, s,
		Entry{Series: old, Points: []Point{{TimeMs: 1_000_000, Sketch: sketchOf(t, 1)}}},
		Entry{Series: fresh, Points: []Point{{TimeMs: 9_000_000, Sketch: sketchOf(t, 1)}}},
	)

	dropped, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped %d series, want 1 — only the one entirely before the cutoff", dropped)
	}
	if got := s.Stats().Series; got != 1 {
		t.Errorf("series after the sweep: got %d, want 1", got)
	}

	keep := open(t, Options{Clock: clk, Retention: -1})
	mustAppend(t, keep, Entry{Series: old, Points: []Point{{TimeMs: 1_000_000, Sketch: sketchOf(t, 1)}}})
	if n, err := keep.Sweep(context.Background()); err != nil || n != 0 {
		t.Errorf("a negative retention swept %d series (%v)", n, err)
	}
	if got := keep.Stats().Series; got != 1 {
		t.Errorf("series: got %d, want 1", got)
	}
}

// Reading a window with no data, or an inverted one, is not an error — the
// query layer asks for windows that predate a series all the time.
func TestStore_ReadingNothing(t *testing.T) {
	s := open(t, Options{})
	r := ref("d", "k:v")
	mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: 5000, Sketch: sketchOf(t, 1)}}})

	for _, tc := range []struct {
		name     string
		from, to int64
	}{
		{"before any data", 0, 1000},
		{"after all data", 10_000, 20_000},
		{"inverted", 10_000, 0},
		{"unknown series", 0, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := r
			if tc.name == "unknown series" {
				target = ref("nothing.here", "k:v")
			}
			got, err := s.Read(context.Background(), target, tc.from, tc.to)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %d points, want none", len(got))
			}
		})
	}
}

// A cancelled context stops a long append or read rather than finishing it.
func TestStore_RespectsContextCancellation(t *testing.T) {
	s := open(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := ref("d", "k:v")
	if _, err := s.Append(ctx, []Entry{{Series: r, Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}}}}); !errors.Is(err, context.Canceled) {
		t.Errorf("Append: got %v, want context.Canceled", err)
	}
	if _, err := s.Truncate(ctx, 1000); !errors.Is(err, context.Canceled) {
		t.Errorf("Truncate: got %v, want context.Canceled", err)
	}
}

// Pebble talks to whatever logger it is given, and the default is stderr.
// Routing it into slog is what keeps a compaction note attributable; Fatalf
// must not return, because Pebble calls it when it has found something it
// cannot continue past.
func TestPebbleLogger(t *testing.T) {
	var buf bytes.Buffer
	l := pebbleLogger{slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	l.Infof("compacting %d files", 3)
	l.Errorf("cannot read %s", "000123.sst")
	out := buf.String()
	if !strings.Contains(out, "compacting 3 files") || !strings.Contains(out, "cannot read 000123.sst") {
		t.Errorf("pebble output did not reach the logger: %q", out)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Fatalf returned; Pebble calls it when continuing is not an option")
			}
		}()
		l.Fatalf("manifest is corrupt")
	}()
}

// A data directory that cannot be opened is a startup failure, not a first-
// write failure: an operator finds out when they start the process, which is
// when they are looking.
func TestStore_OpenFailsLoudlyOnAnUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(Options{Dir: file}); err == nil {
		_ = s.Close()
		t.Fatal("opened a store on a regular file")
	}
}

// The key holds unix seconds in 32 bits, so a query for "everything" has to
// be clamped into that range rather than wrapping into it.
func TestStore_ReadsClampToTheKeyRange(t *testing.T) {
	s := open(t, Options{})
	r := ref("d", "k:v")
	mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: 5_000_000, Sketch: sketchOf(t, 1)}}})

	got, err := s.Read(context.Background(), r, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].TimeMs != 5_000_000 {
		t.Errorf("got %+v, want the one point at 5000000ms", got)
	}
}

// A point at the very top of the key range is still readable: the upper bound
// of a whole-series scan cannot be "the next timestamp", because there isn't
// one.
func TestStore_ThePointAtTheTopOfTheKeyRange(t *testing.T) {
	s := open(t, Options{})
	r := ref("d", "k:v")
	mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: MaxTimestamp * 1000, Sketch: sketchOf(t, 1)}}})

	got, err := s.Read(context.Background(), r, 0, math.MaxInt64)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d points, want the one at the top of the range", len(got))
	}
}

// Keys come off the same disk the values do. A key of the wrong length means
// something wrote bytes this store did not, and reading past it would hand
// the query layer a timestamp assembled from whatever followed.
func TestStore_RejectsMalformedKeys(t *testing.T) {
	t.Run("in the point keyspace", func(t *testing.T) {
		s := open(t, Options{})
		r := ref("d", "k:v")
		mustAppend(t, s, Entry{Series: r, Points: []Point{{TimeMs: 1000, Sketch: sketchOf(t, 1)}}})

		// One byte too long, and inside the scanned range: a key shorter
		// than the format sorts before the lower bound and is never read,
		// which is not the case worth worrying about.
		long := append(pointKey(SeriesID(r), 2), 0xff)
		if err := s.db.Set(long, []byte{codecRaw}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Read(context.Background(), r, 0, 10_000); err == nil {
			t.Fatal("read past a malformed point key")
		}
	})

	t.Run("in the series keyspace, at open", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sketches")
		s := open(t, Options{Dir: dir})
		if err := s.db.Set([]byte{prefixSeries, 1, 2}, []byte("d|k:v"), nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if again, err := Open(Options{Dir: dir}); err == nil {
			_ = again.Close()
			t.Fatal("opened a store whose series index holds a malformed key")
		}
	})
}
