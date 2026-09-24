package db

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/head"
)

// stressSeconds is how long TestDB_Stress runs. The default keeps `make ci`
// honest without making it slow; the 30s of the test plan is
// OZY_STRESS_SECONDS=30, which is what the M2 notes report.
func stressSeconds(t *testing.T) time.Duration {
	if v := os.Getenv("OZY_STRESS_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("OZY_STRESS_SECONDS=%q: %v", v, err)
		}
		return time.Duration(n) * time.Second
	}
	return 5 * time.Second
}

// TestDB_Stress runs everything at once: 16 appenders, 4 selectors, and a
// goroutine cutting and compacting blocks underneath them.
//
// Every other test in this package holds one thing still to look at another.
// This one holds nothing still. It is meant to be run under -race, where it is
// the only test that can catch a lock ordering mistake between the head's
// stripes, the block list and the compaction swap — the three places a reader
// and a writer can meet.
//
// Each sample carries its own timestamp as its value, so a selector can check
// any sample it sees in isolation, without knowing what else was written. The
// count is checked at the end: exactly as many samples must be readable as
// were acknowledged, which no amount of cutting, merging or reopening may
// change.
func TestDB_Stress(t *testing.T) {
	if testing.Short() {
		t.Skip("runs for seconds on purpose")
	}
	testutil.CheckGoroutines(t)

	const (
		appenders       = 16
		selectors       = 4
		seriesPer       = 4
		batch           = 20
		base      int64 = 1_700_000_000_000
	)
	db, _, _ := open(t, Options{
		// Short enough that cuts and compactions run constantly under the
		// readers rather than once at the end.
		BlockRange:    200 * time.Millisecond,
		MaxBlockRange: 2 * time.Second,
		Retention:     -1, // deletion is TestDB_ThreeSimulatedDays' job
		SyncInterval:  20 * time.Millisecond,
	})

	// One shared clock for the data, so every appender writes at roughly the
	// same timestamp. Letting them drift apart would mean a cut for the
	// furthest-ahead appender lands out of bounds for the furthest behind —
	// real behaviour, but it would drown this test's invariant in rejections.
	var tick atomic.Int64
	var acked atomic.Int64
	var oob atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for a := 0; a < appenders; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			refs := make([]tsdb.SeriesRef, seriesPer)
			for i := range refs {
				refs[i] = ref("m", fmt.Sprintf("app:a%d", a), fmt.Sprintf("host:h%d", i))
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := make([]tsdb.SeriesSamples, len(refs))
				for i, r := range refs {
					samples := make([]tsdb.Sample, batch)
					for j := range samples {
						ts := base + tick.Add(1)
						samples[j] = tsdb.Sample{T: ts, V: float64(ts)}
					}
					req[i] = tsdb.SeriesSamples{Series: r, Samples: samples}
				}
				res, err := db.Append(ctx, req)
				if err != nil {
					t.Errorf("appender %d: %v", a, err)
					return
				}
				acked.Add(int64(res.Samples))
				for _, rj := range res.Rejected {
					// A cut can land between an appender reading the tick and
					// its batch reaching the head; anything else is a bug.
					if rj.Reason != reasonOf(head.ErrOutOfBounds) {
						t.Errorf("appender %d: unexpected rejection: %s", a, rj.Reason)
						return
					}
					oob.Add(1)
				}
			}
		}(a)
	}

	for s := 0; s < selectors; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(s))) //nolint:gosec // test data
			for {
				select {
				case <-stop:
					return
				default:
				}
				from := base + rng.Int63n(max64(tick.Load(), 1))
				if !readAndCheck(t, db, tsdb.Selector{Metric: "m"}, from, from+5_000) {
					return
				}
			}
		}(s)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := db.CutBlock(); err != nil && !errors.Is(err, errNothingToCut) {
				t.Errorf("cut: %v", err)
				return
			}
			if _, err := db.Compact(); err != nil {
				t.Errorf("compact: %v", err)
				return
			}
		}
	}()

	time.Sleep(stressSeconds(t))
	close(stop)
	wg.Wait()
	if t.Failed() {
		return
	}

	// Nothing was lost and nothing was doubled, across however many blocks the
	// data ended up spread over.
	total := 0
	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	series := 0
	for set.Next() {
		series++
		key := set.Series().Key()
		it := set.Iterator()
		var last int64 = math.MinInt64
		for it.Next() {
			s := it.At()
			if s.T <= last {
				t.Fatalf("%s: %d follows %d", key, s.T, last)
			}
			if s.V != float64(s.T) {
				t.Fatalf("%s: sample at %d has value %g", key, s.T, s.V)
			}
			last = s.T
			total++
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if series != appenders*seriesPer {
		t.Errorf("read back %d series, wrote %d", series, appenders*seriesPer)
	}
	if int64(total) != acked.Load() {
		t.Errorf("%d samples readable, %d acknowledged", total, acked.Load())
	}
	t.Logf("%s: %d samples acknowledged and readable across %d series and %d blocks (%d out of bounds)",
		stressSeconds(t), total, series, len(db.Blocks()), oob.Load())
}

// readAndCheck runs one query and validates every sample it returns on its
// own terms. It reports whether to keep going.
func readAndCheck(t *testing.T, db *DB, sel tsdb.Selector, from, to int64) bool {
	t.Helper()
	set, err := db.Select(ctx, sel, from, to)
	if err != nil {
		t.Errorf("select: %v", err)
		return false
	}
	defer func() { _ = set.Close() }()
	for set.Next() {
		key := set.Series().Key()
		it := set.Iterator()
		var last int64 = math.MinInt64
		for it.Next() {
			s := it.At()
			switch {
			case s.T <= last:
				t.Errorf("%s: %d follows %d", key, s.T, last)
				return false
			case s.T < from || s.T > to:
				t.Errorf("%s: %d is outside the window [%d, %d]", key, s.T, from, to)
				return false
			case s.V != float64(s.T):
				t.Errorf("%s: sample at %d has value %g", key, s.T, s.V)
				return false
			}
			last = s.T
		}
		if err := it.Err(); err != nil {
			t.Errorf("iterating: %v", err)
			return false
		}
	}
	if err := set.Err(); err != nil {
		t.Errorf("set: %v", err)
		return false
	}
	return true
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
