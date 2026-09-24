package block

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// goldenBlock is the fixture behind testdata/golden/block/: three series
// sharing a metric name and an environment tag, so the symbol table has
// something to do, and one of them carrying a tag the others do not, so the
// postings lists are not all identical.
func goldenBlock() []tsdb.SeriesSamples {
	var out []tsdb.SeriesSamples
	for i := 0; i < 3; i++ {
		tags := []string{"env:prod", fmt.Sprintf("host:h%d", i)}
		if i == 2 {
			tags = append(tags, "canary:true")
		}
		var samples []tsdb.Sample
		for j := 0; j < 5; j++ {
			samples = append(samples, tsdb.Sample{
				T: 1600000000000 + int64(j)*10_000,
				V: float64(i*100 + j),
			})
		}
		out = append(out, tsdb.SeriesSamples{
			Series:  tsdb.NewSeriesRef("http.request.count", tags),
			Samples: samples,
		})
	}
	return out
}

// TestBlock_Golden is the promise that a block written months ago still opens.
//
// Unlike the chunk and the log, this one does not compare bytes: a block
// carries a ULID and a creation time, so two runs never produce identical
// files. What has to hold is that the committed directory — its symbol table,
// its sorted offset table, its delta-varint postings, its TOC and its
// checksums — is still readable, and answers the same queries with the same
// data. That is the guarantee an operator cares about after an upgrade.
//
// Regenerate with -update-golden. Note that the directory is named `block`
// rather than by its ULID: [Open] reads a path, and a fixed name keeps the
// diff legible.
func TestBlock_Golden(t *testing.T) {
	dir := filepath.Join("testdata", "golden", "block")
	want := goldenBlock()

	if *updateGolden {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		parent := t.TempDir()
		meta, err := Write(parent, want, WriterOptions{Now: epoch})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(parent, meta.ULID.String()), dir); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (ulid %s)", dir, meta.ULID)
	}

	b, err := Open(dir)
	if err != nil {
		t.Fatalf("the committed block no longer opens: %v\n"+
			"regenerate with: go test ./internal/tsdb/block -run Golden -update-golden", err)
	}
	defer func() { _ = b.Close() }()

	m := b.Meta()
	if m.Version != MetaVersion {
		t.Errorf("committed block is version %d, this build reads %d", m.Version, MetaVersion)
	}
	if m.Stats.Series != len(want) {
		t.Errorf("meta says %d series, the fixture has %d", m.Stats.Series, len(want))
	}

	got, err := b.Select(tsdb.Selector{Metric: "http.request.count"}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatalf("selecting from the committed block: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d series, want %d", len(got), len(want))
	}
	byKey := map[string][]tsdb.Sample{}
	for _, s := range got {
		byKey[s.Series.Key()] = s.Samples
	}
	for _, w := range want {
		samples, ok := byKey[w.Series.Key()]
		if !ok {
			t.Errorf("series %s is missing from the committed block", w.Series.Key())
			continue
		}
		if len(samples) != len(w.Samples) {
			t.Errorf("%s has %d samples, want %d", w.Series.Key(), len(samples), len(w.Samples))
			continue
		}
		for i, s := range samples {
			if s != w.Samples[i] {
				t.Errorf("%s sample %d is %+v, want %+v", w.Series.Key(), i, s, w.Samples[i])
			}
		}
	}

	// The index is the part most likely to rot silently, so ask it something.
	canary, err := b.Select(tsdb.Selector{
		Metric:   "http.request.count",
		Matchers: []tsdb.Matcher{{Key: "canary", Value: "true", Type: tsdb.Equal}},
	}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if len(canary) != 1 {
		t.Errorf("the committed block's postings returned %d series for canary:true, want 1", len(canary))
	}
}
