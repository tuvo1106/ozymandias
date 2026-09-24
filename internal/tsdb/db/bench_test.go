package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

func benchRefs(n int) []tsdb.SeriesRef {
	refs := make([]tsdb.SeriesRef, n)
	for i := range refs {
		refs[i] = tsdb.NewSeriesRef("http.request.count",
			[]string{"env:prod", "service:checkout", fmt.Sprintf("host:h%d", i)})
	}
	return refs
}

// BenchmarkDB_Append is the number the acceptance criterion is about: samples
// per second through the whole store.
//
// With sync-on-append the answer is almost entirely "how many samples share
// one fsync", which is why batch size is a dimension and goroutine count
// barely is. An fsync costs milliseconds whatever it flushes, and it happens
// inside the commit lock, so a second appender does not overlap with the
// first — it queues behind it. Batching is the only lever, and it is a big
// one. Without sync-on-append the log write is a buffered copy and the
// store's own cost is what is left.
func BenchmarkDB_Append(b *testing.B) {
	type shape struct {
		sync       bool
		batch      int
		goroutines int
	}
	shapes := []shape{
		{true, 100, 1}, {true, 1000, 1}, {true, 10_000, 1},
		{true, 1000, 8}, {true, 1000, 64},
		{false, 1000, 1}, {false, 1000, 8}, {false, 1000, 64},
	}
	for _, sh := range shapes {
		name := fmt.Sprintf("sync=%v/batch=%d/goroutines=%d", sh.sync, sh.batch, sh.goroutines)
		b.Run(name, func(b *testing.B) {
			db, _, _ := openBench(b, Options{
				BlockRange:   time.Hour,
				Retention:    -1,
				SyncOnAppend: sh.sync,
			})
			refs := benchRefs(sh.batch)
			var clock atomic.Int64

			b.SetParallelism(sh.goroutines)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				batch := make([]tsdb.SeriesSamples, len(refs))
				for pb.Next() {
					t := clock.Add(1) * 10
					for i, r := range refs {
						batch[i] = tsdb.SeriesSamples{
							Series:  r,
							Samples: []tsdb.Sample{{T: t, V: float64(i)}},
						}
					}
					if _, err := db.Append(ctx, batch); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			// One iteration is a whole batch; per-sample is the number that
			// compares to anything else.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*sh.batch), "ns/sample")
			b.ReportMetric(float64(b.N*sh.batch)/b.Elapsed().Seconds(), "samples/sec")
		})
	}
}

// BenchmarkDB_Replay measures startup: how long a restart takes per byte of
// write-ahead log, which is what decides how long an ozyd is down after a
// crash. The log is built once and replayed b.N times.
func BenchmarkDB_Replay(b *testing.B) {
	dir := b.TempDir()
	db, err := Open(Options{Dir: dir, BlockRange: 1 << 40, Retention: -1})
	if err != nil {
		b.Fatal(err)
	}
	refs := benchRefs(2000)
	batch := make([]tsdb.SeriesSamples, len(refs))
	for t := int64(0); t < 120; t++ {
		for i, r := range refs {
			batch[i] = tsdb.SeriesSamples{Series: r, Samples: []tsdb.Sample{{T: t * 10_000, V: float64(i)}}}
		}
		if _, err := db.Append(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	size := walBytes(b, dir)
	b.ReportMetric(float64(size), "wal-bytes")

	b.ResetTimer()
	for b.Loop() {
		reopened, err := Open(Options{Dir: dir, BlockRange: 1 << 40, Retention: -1})
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := reopened.Close(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	seconds := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(float64(size)/(1<<30)/seconds, "GiB/sec")
}

func walBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	var total int64
	entries, err := os.ReadDir(filepath.Join(dir, walDirName))
	if err != nil {
		tb.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			tb.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// openBench is [open] for a benchmark: testing.B is not a *testing.T.
func openBench(b *testing.B, opts Options) (*DB, string, string) {
	b.Helper()
	if opts.Dir == "" {
		opts.Dir = b.TempDir()
	}
	db, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db, "", opts.Dir
}

// TestDB_ReplayOneGiB is the acceptance criterion "a restart with a 1 GiB
// write-ahead log replays in under 30 seconds", measured rather than
// extrapolated: replay cost is not linear in bytes, because the head is
// rebuilding chunks and an index as it goes, and a small log never shows that.
//
// It writes a gigabyte and holds the result in memory, so it runs only when
// asked: OZY_BIG_REPLAY=1 go test ./internal/tsdb/db/ -run OneGiB -v
func TestDB_ReplayOneGiB(t *testing.T) {
	if os.Getenv("OZY_BIG_REPLAY") == "" {
		t.Skip("set OZY_BIG_REPLAY=1: writes 1 GiB and replays it")
	}
	const target = int64(1) << 30
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, BlockRange: 1 << 40, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	refs := benchRefs(10_000)
	batch := make([]tsdb.SeriesSamples, len(refs))
	start := time.Now()
	var samples int64
	for ts := int64(0); walBytes(t, dir) < target; ts++ {
		for i, r := range refs {
			batch[i] = tsdb.SeriesSamples{Series: r, Samples: []tsdb.Sample{{T: ts * 10_000, V: float64(i) + float64(ts)}}}
		}
		res, err := db.Append(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		samples += int64(res.Samples)
	}
	size := walBytes(t, dir)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %.2f GiB (%d samples across %d series) in %s",
		float64(size)/(1<<30), samples, len(refs), time.Since(start).Round(time.Millisecond))

	start = time.Now()
	reopened, err := Open(Options{Dir: dir, BlockRange: 1 << 40, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	st := reopened.HeadStats()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("replayed %.2f GiB in %s (%.2f GiB/sec, %d samples restored)",
		float64(size)/(1<<30), elapsed.Round(time.Millisecond),
		float64(size)/(1<<30)/elapsed.Seconds(), st.Samples)
	if st.Samples != samples {
		t.Errorf("restored %d samples, acknowledged %d", st.Samples, samples)
	}
	if elapsed > 30*time.Second {
		t.Errorf("replay took %s, the criterion is under 30s", elapsed)
	}
}
