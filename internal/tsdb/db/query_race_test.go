package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// TestDB_AQueryNeverFallsIntoTheGapBetweenTheHeadAndANewBlock.
//
// A block cut moves samples from memory to disk. [DB.CutBlock] orders that
// move so a reader can never miss them: the block is published to db.blocks
// *before* the head is truncated, so for a moment the samples are in both
// places and a query that sees both just deduplicates.
//
// That ordering only protects a reader that looks at the head first. Select
// looked at the blocks first — it snapshotted db.blocks, spent however long it
// takes to read them, and only then read the head. A cut landing in that
// window publishes its block after the snapshot was taken and truncates the
// head before the head is read, so the range it just moved is in neither half
// of the answer. The query succeeds, and silently returns a hole.
//
// Nothing about it is exotic: one cut inside one query. It needs only that
// reading the blocks takes longer than the gap between those two steps, which
// on any database with real blocks in it, it does.
//
// No appends here. The data is written first and then left alone, so the
// number of samples in the store is a constant and any answer other than that
// constant is the bug — no reasoning about what was in flight, and nothing to
// make the assertion timing-dependent. Only *reaching* the window is timing;
// the window is the whole block-read phase, which is why a handful of rounds
// is enough.
func TestDB_AQueryNeverFallsIntoTheGapBetweenTheHeadAndANewBlock(t *testing.T) {
	const (
		nSeries    = 4
		totalTs    = 120_000
		blockRange = 10 * time.Second
		readers    = 4
	)
	db, _, _ := open(t, Options{BlockRange: blockRange, Retention: -1})

	refs := make([]tsdb.SeriesRef, nSeries)
	for i := range refs {
		refs[i] = ref("m", fmt.Sprintf("host:h%d", i))
	}
	batch := make([]tsdb.SeriesSamples, nSeries)
	var acked int64
	for ts := int64(0); ts < totalTs; ts++ {
		for i, r := range refs {
			batch[i] = tsdb.SeriesSamples{Series: r, Samples: []tsdb.Sample{{T: ts, V: float64(ts)}}}
		}
		res, err := db.Append(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		acked += int64(res.Samples)
	}

	// Cut everything the head will give up, one range at a time, while the
	// readers run. Each call moves one block range from memory to disk.
	cuts := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(cuts)
		for {
			err := db.CutBlock()
			if errors.Is(err, errNothingToCut) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-cuts:
					return
				default:
				}
				got, err := countSamplesErr(db)
				if err != nil {
					t.Error(err)
					return
				}
				if got != acked {
					t.Errorf("a query returned %d samples, want %d — it fell into the gap "+
						"between a block being published and the head being truncated", got, acked)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := countSamples(t, db); got != acked {
		t.Errorf("%d samples once everything has settled, want %d", got, acked)
	}
}

// TestDB_AMetadataQueryNeverFallsIntoTheSameGap is the metadata half of
// TestDB_AQueryNeverFallsIntoTheGapBetweenTheHeadAndANewBlock. Sample reads
// and metadata reads combine the head and the blocks separately, so fixing the
// order in one says nothing about the other.
//
// A series that reports for one block range and then goes quiet is the one
// this can lose: it lives in exactly one block, and in the head only until
// that block is cut. Every other series is in several sources at once, so
// dropping a source still leaves it visible and the bug hides. Real fleets are
// full of the first kind — a short-lived container is exactly this shape.
func TestDB_AMetadataQueryNeverFallsIntoTheSameGap(t *testing.T) {
	const (
		ranges     = 12
		perRange   = 10_000
		blockRange = 10 * time.Second
		readers    = 4
	)
	db, _, _ := open(t, Options{BlockRange: blockRange, Retention: -1})

	// One series per range, present only in that range, plus a carrier that
	// spans everything so there is always something left to cut.
	want := map[string]bool{"carrier": true}
	for r := 0; r < ranges; r++ {
		host := fmt.Sprintf("only%d", r)
		want[host] = true
		for ts := int64(r) * perRange; ts < int64(r+1)*perRange; ts++ {
			_, err := db.Append(ctx, []tsdb.SeriesSamples{
				{Series: ref("m", "host:"+host), Samples: []tsdb.Sample{{T: ts, V: 1}}},
				{Series: ref("m", "host:carrier"), Samples: []tsdb.Sample{{T: ts, V: 1}}},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	cuts := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(cuts)
		for {
			err := db.CutBlock()
			if errors.Is(err, errNothingToCut) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-cuts:
					return
				default:
				}
				got, err := db.TagValues(ctx, "m", "host", 0)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) != len(want) {
					t.Errorf("host has %d values during a cut, want %d: %v — a series was in "+
						"neither the block snapshot nor the head", len(got), len(want), got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
