package db_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/db"
)

// The store from the outside: open a directory, append a batch, read it back.
// This is the whole of [tsdb.MetricStore] as ozyd uses it — the log, the
// head, the blocks and the index are all behind these three calls.
func Example() {
	dir, err := os.MkdirTemp("", "tsdb-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	store, err := db.Open(db.Options{
		Dir:        dir,
		BlockRange: 2 * time.Hour,
		Retention:  -1, // keep everything, for an example with old timestamps
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	base := int64(1600000000000)
	res, err := store.Append(ctx, []tsdb.SeriesSamples{
		{
			Series: tsdb.NewSeriesRef("http.request.count", []string{"env:prod", "route:/api"}),
			Samples: []tsdb.Sample{
				{T: base, V: 1}, {T: base + 10_000, V: 4}, {T: base + 20_000, V: 9},
			},
		},
		{
			Series:  tsdb.NewSeriesRef("http.request.count", []string{"env:dev", "route:/api"}),
			Samples: []tsdb.Sample{{T: base, V: 2}},
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("stored %d samples across %d series\n", res.Samples, res.Series)

	// A sample the store already has is refused rather than overwriting one:
	// chunks are append-only (ADR-0011).
	res, err = store.Append(ctx, []tsdb.SeriesSamples{{
		Series:  tsdb.NewSeriesRef("http.request.count", []string{"env:prod", "route:/api"}),
		Samples: []tsdb.Sample{{T: base, V: 999}},
	}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("backdated write: stored %d, rejected %d (%s)\n",
		res.Samples, len(res.Rejected), res.Rejected[0].Reason)

	// Selecting narrows by metric, by tag and by time, in that order.
	set, err := store.Select(ctx, tsdb.Selector{
		Metric:   "http.request.count",
		Matchers: []tsdb.Matcher{{Key: "env", Value: "prod", Type: tsdb.Equal}},
	}, base, base+15_000)
	if err != nil {
		panic(err)
	}
	defer func() { _ = set.Close() }()
	for set.Next() {
		fmt.Printf("%s:", set.Series().Key())
		it := set.Iterator()
		for it.Next() {
			s := it.At()
			fmt.Printf(" %d=%g", s.T-base, s.V)
		}
		fmt.Println()
	}
	if err := set.Err(); err != nil {
		panic(err)
	}
	// Output:
	// stored 4 samples across 2 series
	// backdated write: stored 0, rejected 1 (sample at or before the series' newest timestamp)
	// http.request.count|env:prod,route:/api: 0=1 10000=4
}
