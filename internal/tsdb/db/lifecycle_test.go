package db

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// TestDB_ThreeSimulatedDays runs the whole storage lifecycle against a fake
// clock: samples arrive, ranges fill, blocks are cut, adjacent blocks are
// compacted into wider ones, and blocks older than the retention window are
// deleted — for 72 hours of simulated time in a couple of seconds.
//
// The scheduling is real: every timestamp, every retention decision and every
// range boundary comes from the fake clock, three days of them. What the test
// drives by hand is only the maintenance goroutine's *wakeups*, calling the
// same three methods in the same order it does, because a background loop
// racing 432 assertions would test the scheduler rather than the lifecycle.
// TestDB_MaintenanceRunsOnTheClock covers the wiring to the ticker.
//
// The invariant that matters is at the bottom: after three days of cutting,
// merging and deleting, everything inside the retention window is still
// there, exactly once, and nothing the store returns was ever invented.
func TestDB_ThreeSimulatedDays(t *testing.T) {
	const (
		blockRange = 2 * time.Hour
		retention  = 24 * time.Hour
		step       = 10 * time.Minute
		days       = 3
		series     = 4
	)
	db, fake, _ := open(t, Options{
		BlockRange: blockRange,
		Retention:  retention,
		// Default (54h) would let a level-3 block outlive the window it was
		// merged from; pinned here so the shape of the run is stated, not
		// inherited.
		MaxBlockRange: 18 * time.Hour,
	})

	refs := make([]tsdb.SeriesRef, series)
	for i := range refs {
		refs[i] = ref("m", fmt.Sprintf("host:h%d", i))
	}
	// Everything acknowledged, by series key, so the end state can be checked
	// against what the store was actually told.
	want := map[string][]tsdb.Sample{}
	seen := map[string]bool{} // every block ulid that ever existed
	maxLevel := 0
	var cuts, compactions, deletions int

	for elapsed := time.Duration(0); elapsed < days*24*time.Hour; elapsed += step {
		fake.Advance(step)
		now := fake.Now().UnixMilli()

		batch := make([]tsdb.SeriesSamples, series)
		for i, r := range refs {
			batch[i] = tsdb.SeriesSamples{Series: r, Samples: []tsdb.Sample{sm(now, float64(i))}}
		}
		res, err := db.Append(ctx, batch)
		if err != nil {
			t.Fatalf("append at %s: %v", fake.Now(), err)
		}
		if res.Samples != series {
			t.Fatalf("at %s stored %d of %d samples: %+v", fake.Now(), res.Samples, series, res.Rejected)
		}
		for i, r := range refs {
			want[r.Key()] = append(want[r.Key()], sm(now, float64(i)))
		}

		before := len(db.Blocks())
		if err := db.CutBlock(); err != nil && !errors.Is(err, errNothingToCut) {
			t.Fatalf("cut at %s: %v", fake.Now(), err)
		}
		if len(db.Blocks()) > before {
			cuts++
		}
		did, err := db.Compact()
		if err != nil {
			t.Fatalf("compact at %s: %v", fake.Now(), err)
		}
		if did {
			compactions++
		}
		if err := db.ApplyRetention(); err != nil {
			t.Fatalf("retention at %s: %v", fake.Now(), err)
		}
		for _, b := range db.Blocks() {
			m := b.Meta()
			seen[m.ULID.String()] = true
			if m.Compaction.Level > maxLevel {
				maxLevel = m.Compaction.Level
			}
		}
	}
	deletions = len(seen) - len(db.Blocks())

	// Three days at a 2h range is 36 ranges; the head holds back a half range
	// of landing room, so the last one or two are still in memory.
	if cuts < 33 {
		t.Errorf("cut %d blocks in 3 days, expected ~35", cuts)
	}
	if compactions == 0 || maxLevel < 2 {
		t.Errorf("%d compactions, deepest level %d: nothing was merged twice", compactions, maxLevel)
	}
	if deletions == 0 {
		t.Errorf("retention never deleted anything: %d blocks ever, %d now", len(seen), len(db.Blocks()))
	}
	// The point of compaction: file count grows with the log of the data, not
	// with it. 36 cuts merged 3-at-a-time and expired at 24h leaves a handful.
	if n := len(db.Blocks()); n > 16 {
		t.Errorf("%d blocks after 3 days: compaction is not keeping up", n)
	}

	// Now the data. Everything inside the retention window must still be
	// readable, exactly once, and nothing outside what was appended may come
	// back at all.
	cutoff := fake.Now().Add(-retention).UnixMilli()
	got := map[string][]tsdb.Sample{}
	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	for set.Next() {
		key := set.Series().Key()
		it := set.Iterator()
		var last int64 = math.MinInt64
		for it.Next() {
			s := it.At()
			if s.T <= last {
				t.Fatalf("%s: sample at %d follows %d — out of order or duplicated", key, s.T, last)
			}
			last = s.T
			got[key] = append(got[key], s)
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
	if len(got) != series {
		t.Fatalf("read back %d series, appended %d", len(got), series)
	}
	for key, appended := range want {
		index := map[int64]float64{}
		for _, s := range got[key] {
			index[s.T] = s.V
		}
		var missing, wrong int
		for _, s := range appended {
			v, ok := index[s.T]
			switch {
			case !ok && s.T >= cutoff:
				missing++
			case ok && v != s.V:
				wrong++
			}
			delete(index, s.T)
		}
		if missing > 0 {
			t.Errorf("%s: %d samples inside the retention window are gone", key, missing)
		}
		if wrong > 0 {
			t.Errorf("%s: %d samples came back with the wrong value", key, wrong)
		}
		if len(index) > 0 {
			t.Errorf("%s: %d samples came back that were never appended", key, len(index))
		}
	}
	t.Logf("3 days: %d cuts, %d compactions (deepest level %d), %d blocks deleted, %d left",
		cuts, compactions, maxLevel, deletions, len(db.Blocks()))
}
