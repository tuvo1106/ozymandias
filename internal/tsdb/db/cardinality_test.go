package db

import (
	"fmt"
	"testing"
	"time"
)

// The cardinality counts are of distinct series, and the TSDB keeps one
// series in several places: still being written after a cut, it is in the
// block and the head. Summing the sources' postings would count it twice —
// which is what DB.Stats does — and on the page whose whole point is the
// number.
func TestDB_CardinalityCountsASeriesOnceWhereverItLives(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95) // crosses the cut: block and head
	fill(t, db, ref("m", "env:dev"), 0, 1000, 30)  // before it: block only
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 1 {
		t.Fatal("expected a block")
	}
	// After it: head only. Two values for one key, and a bare tag.
	fill(t, db, ref("m", "env:a", "env:b", "canary"), 100_000, 1000, 3)
	fill(t, db, ref("other"), 100_000, 1000, 3)

	counts, err := db.SeriesCounts(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%+v", counts), "[{Metric:m Series:3} {Metric:other Series:1}]"; got != want {
		t.Errorf("SeriesCounts = %s, want %s", got, want)
	}

	card, err := db.TagCardinality(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	// env: carried by all three series, once each though one carries it
	// twice; four values. canary: one series, no value.
	if got, want := fmt.Sprintf("%+v", card), "[{Key:canary Series:1 Values:0} {Key:env Series:3 Values:4}]"; got != want {
		t.Errorf("TagCardinality = %s, want %s", got, want)
	}

	if got, err := db.TagCardinality(ctx, "missing"); err != nil || len(got) != 0 {
		t.Errorf("TagCardinality(missing) = %v, %v; want empty", got, err)
	}
	if got, err := db.SeriesCounts(ctx, "o"); err != nil || fmt.Sprintf("%+v", got) != "[{Metric:other Series:1}]" {
		t.Errorf("SeriesCounts(o) = %+v, %v", got, err)
	}
}
