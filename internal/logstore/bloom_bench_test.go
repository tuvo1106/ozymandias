package logstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// benchCorpus fills a store with realistic-looking request logs: a few hundred
// distinct message templates, ids that are different every time, and one rare
// token in a handful of places. The store is sealed into small blocks so there
// are many to skip.
func benchCorpus(tb testing.TB, dir string, blooms bool) (*Store, []string) {
	tb.Helper()
	saved := newChunkVersion
	if !blooms {
		newChunkVersion = 1
	}
	defer func() { newChunkVersion = saved }()
	s, _ := openStore(tb, dir, func(o *Options) { o.BlockBytes = 64 << 10 })
	rng := rand.New(rand.NewPCG(7, 8))
	templates := []string{
		"GET %s %d in %dms", "POST %s %d in %dms", "cache miss for key %s (%d) after %dms", "db query %s returned %d rows in %dms",
		"user %s signed in from %d.%d", "job %s finished with %d items in %dms", "retrying %s attempt %d after %dms",
	}
	routes := []string{"/api/orders", "/api/users", "/api/items", "/healthz", "/api/search", "/api/cart", "/login", "/logout"}
	var rare []string
	var batch []wire.Log
	base := t0.UnixMilli()
	for i := 0; i < 60_000; i++ {
		msg := fmt.Sprintf(templates[rng.IntN(len(templates))], routes[rng.IntN(len(routes))], 200+rng.IntN(300), rng.IntN(900))
		l := wire.Log{Ts: base + int64(i)*50, Service: "web-api", Status: "info", Host: "box", Message: msg,
			Attrs: map[string]any{"request_id": fmt.Sprintf("req-%08x", rng.Uint32()), "user": fmt.Sprintf("user%d", rng.IntN(500))}}
		if i%6000 == 3000 {
			tok := fmt.Sprintf("incident-%04d", rng.IntN(10000))
			l.Message += " " + tok
			rare = append(rare, tok)
		}
		batch = append(batch, l)
		if len(batch) == 500 {
			if err := s.Append(context.Background(), batch); err != nil {
				tb.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if err := s.Flush(); err != nil {
		tb.Fatal(err)
	}
	return s, rare
}

func chunkBytes(tb testing.TB, dir string) (total, blooms int64) {
	for _, p := range findFiles(tb, dir, ".chunk") {
		data, _ := os.ReadFile(p)
		ix, err := readIndex(bytesReader(data), int64(len(data)))
		if err != nil {
			tb.Fatal(err)
		}
		total += int64(len(data))
		for _, m := range ix.Blocks {
			blooms += int64(m.BloomLen)
		}
	}
	return total, blooms
}

// TestBloom_MeasuredSkipRateAndOverhead records the numbers docs/notes/M4.md
// reports, and asserts the claims they back: a rare word reads a small fraction
// of the blocks, a common one skips nothing, and the filters cost a bounded
// share of the disk.
func TestBloom_MeasuredSkipRateAndOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 60k-log corpus")
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	with, rare := benchCorpus(t, dirA, true)
	without, _ := benchCorpus(t, dirB, false)
	defer func() { _ = with.Close(); _ = without.Close() }()
	u := with.Usage()
	t.Logf("compression: %d logs, %d raw bytes -> %d compressed (%.1fx), plus %d bytes of filters",
		u.Entries, u.RawBytes, u.CompressedBytes, float64(u.RawBytes)/float64(u.CompressedBytes), u.BloomBytes)
	totalA, bloomsA := chunkBytes(t, dirA)
	totalB, _ := chunkBytes(t, dirB)
	t.Logf("disk: %d bytes with filters, %d without; the filters are %d bytes (%.1f%% of the chunk files)",
		totalA, totalB, bloomsA, 100*float64(bloomsA)/float64(totalA))
	if share := float64(bloomsA) / float64(totalA); share > 0.30 {
		t.Errorf("the filters are %.0f%% of the disk; the design budget is 30%%", share*100)
	}
	run := func(s *Store, q string) *SearchResult {
		res, err := s.Search(context.Background(), mustParse(t, q), wideFrom, wideTo, SearchOpts{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, q := range []string{rare[0], "incident-9999", "user7 signed", "db query", "retrying", `"cache miss for key /api/cart"`, "ab", "NOPE-NOT-THERE"} {
		start := time.Now()
		a := run(with, q)
		ta := time.Since(start)
		start = time.Now()
		b := run(without, q)
		tb := time.Since(start)
		if len(a.Logs) != len(b.Logs) {
			t.Fatalf("%q: %d logs with filters, %d without", q, len(a.Logs), len(b.Logs))
		}
		blocks := a.Stats.BlocksRead + a.Stats.BlocksSkipped
		t.Logf("%-38q %5d logs | with: read %3d skipped %3d of %3d blocks (%5.1f%% skipped) %8v | without: read %3d %8v | %.1fx",
			q, len(a.Logs), a.Stats.BlocksRead, a.Stats.BlocksSkipped, blocks, 100*float64(a.Stats.BlocksSkipped)/float64(max(blocks, 1)), ta.Round(time.Microsecond),
			b.Stats.BlocksRead, tb.Round(time.Microsecond), float64(tb)/float64(max(ta, 1)))
	}
	a := run(with, rare[0])
	if blocks := a.Stats.BlocksRead + a.Stats.BlocksSkipped; a.Stats.BlocksSkipped*10 < blocks*9 {
		t.Errorf("a rare word read %d of %d blocks; expected to skip at least 90%%", a.Stats.BlocksRead, blocks)
	}
	if c := run(with, "retrying"); c.Stats.BlocksSkipped != 0 {
		t.Errorf("a word in every block skipped %d blocks", c.Stats.BlocksSkipped)
	}
}

func BenchmarkSearch_RareWord(b *testing.B) {
	for _, blooms := range []bool{false, true} {
		name := "without-filters"
		if blooms {
			name = "with-filters"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			s, rare := benchCorpus(b, dir, blooms)
			defer func() { _ = s.Close() }()
			q := mustParse(b, rare[0])
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Search(context.Background(), q, wideFrom, wideTo, SearchOpts{Limit: 100}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAppend is the write path with the WAL fsync off (NoSync), so it
// measures the store's own work: encoding, the head, the WAL write. A real
// deployment adds one fsync per request (docs/notes/M4.md has that number).
func BenchmarkAppend(b *testing.B) {
	s, _ := openStore(b, b.TempDir(), func(o *Options) { o.BlockBytes = 256 << 10 })
	defer func() { _ = s.Close() }()
	batch := make([]wire.Log, 500)
	for i := range batch {
		batch[i] = wire.Log{Ts: t0.UnixMilli() + int64(i), Service: "web-api", Status: "info", Host: "box",
			Message: fmt.Sprintf("GET /api/orders/%d 200 in %dms", i, i%900), Attrs: map[string]any{"request_id": fmt.Sprintf("req-%08x", i*7919), "user": "bob"}}
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(batch)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Append(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*len(batch))/b.Elapsed().Seconds(), "logs/s")
}

// BenchmarkAppend_Fsync is the same write with the WAL fsync on: what one
// acknowledged request costs on this disk, which is what bounds a single
// agent's batch rate (the agent sends one batch per poll).
func BenchmarkAppend_Fsync(b *testing.B) {
	s, _ := openStore(b, b.TempDir(), func(o *Options) { o.BlockBytes = 256 << 10; o.NoSync = false })
	defer func() { _ = s.Close() }()
	batch := make([]wire.Log, 500)
	for i := range batch {
		batch[i] = wire.Log{Ts: t0.UnixMilli() + int64(i), Service: "web-api", Status: "info", Host: "box",
			Message: fmt.Sprintf("GET /api/orders/%d 200 in %dms", i, i%900), Attrs: map[string]any{"request_id": fmt.Sprintf("req-%08x", i*7919), "user": "bob"}}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Append(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*len(batch))/b.Elapsed().Seconds(), "logs/s")
}
