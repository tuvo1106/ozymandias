package logstore

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Aggregate and Facets count the same logs a search returns. Over a random
// corpus (several days, blocks, a head) they must equal counts made directly
// from the logs with the whole-query filter.
func TestProperty_AggregateAndFacetsEqualBruteForce(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s, _ := openStore(t, mustTemp(t))
		defer func() { _ = s.Close() }()
		base := t0.UnixMilli()
		var logs []wire.Log
		for b := 0; b < rapid.IntRange(1, 6).Draw(t, "batches"); b++ {
			var batch []wire.Log
			for i := 0; i < rapid.IntRange(1, 25).Draw(t, fmt.Sprintf("n%d", b)); i++ {
				l := randLog(t, base, fmt.Sprintf("l%d.%d", b, i))
				batch = append(batch, l)
				logs = append(logs, l)
			}
			if err := s.Append(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			if rapid.Bool().Draw(t, fmt.Sprintf("flush%d", b)) {
				_ = s.Flush()
			}
		}
		q := randQuery(t, "q")
		node := mustParse(t, q)
		f := logql.Compile(node)
		from, to := base-3*86_400_000, base+3*86_400_000
		var match []wire.Log
		for _, l := range logs {
			if l.Ts >= from && l.Ts <= to && f(&l) {
				match = append(match, l)
			}
		}

		by := rapid.SampledFrom([]string{"", "status", "service", "@code", "@ms", "env"}).Draw(t, "by")
		step := rapid.SampledFrom([]time.Duration{time.Second, time.Minute, time.Hour}).Draw(t, "step")
		res, err := s.Aggregate(context.Background(), node, from, to, AggSpec{Interval: step, By: by})
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]map[string]int64{}
		for _, l := range match {
			ts := l.Ts - mod(l.Ts, step.Milliseconds())
			if want[ts] == nil {
				want[ts] = map[string]int64{}
			}
			want[ts][groupValue(&l, by)]++
		}
		got := map[int64]map[string]int64{}
		prev := int64(-1) << 62
		for _, b := range res.Buckets {
			if b.Ts <= prev {
				t.Fatalf("buckets are not in strictly increasing order: %d after %d", b.Ts, prev)
			}
			prev = b.Ts
			got[b.Ts] = b.Counts
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("query %q by %q every %v:\n got  %v\n want %v", q, by, step, got, want)
		}

		fr, err := s.Facets(context.Background(), node, from, to, []string{"status", "@code"}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"status", "@code"} {
			wantC := map[string]int64{}
			for _, l := range match {
				for _, v := range facetValues(&l, key) {
					wantC[v]++
				}
			}
			gotC := map[string]int64{}
			for _, fc := range fr.Facets[key] {
				gotC[fc.Value] = fc.Count
			}
			if fmt.Sprint(gotC) != fmt.Sprint(wantC) {
				t.Fatalf("facet %s for %q: got %v want %v", key, q, gotC, wantC)
			}
		}
	})
}

// mustTemp makes a directory that outlives nothing: rapid runs the body many
// times, so it cannot use t.TempDir.
func mustTemp(t *rapid.T) string {
	dir, err := os.MkdirTemp("", "logstore-prop")
	if err != nil {
		t.Fatal(err)
	}
	tempDirs = append(tempDirs, dir)
	return dir
}

var tempDirs []string

func TestAggregate_RejectsWhatItCannotDo(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.Aggregate(ctx, nil, 0, 10, AggSpec{Interval: 0}); err == nil {
		t.Error("a zero interval was accepted")
	}
	if _, err := s.Aggregate(ctx, nil, 0, 10, AggSpec{Interval: time.Second, By: "trace_idx"}); err == nil {
		t.Error("an unknown group-by was accepted")
	}
	if _, err := s.Facets(ctx, nil, 0, 10, []string{"nope"}, 5); err == nil {
		t.Error("an unknown facet key was accepted")
	}
}

func TestAggregate_GroupsPastTheCapFoldIntoOther(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	var batch []wire.Log
	for i := 0; i < MaxGroups+50; i++ {
		l := mkLog(base, "api", "info", "m", 0)
		l.Attrs["user"] = fmt.Sprintf("u%d", i)
		batch = append(batch, l)
	}
	_ = s.Append(context.Background(), batch)
	res, err := s.Aggregate(context.Background(), nil, base-1, base+1, AggSpec{Interval: time.Hour, By: "@user"})
	if err != nil {
		t.Fatal(err)
	}
	counts := res.Buckets[0].Counts
	if len(counts) != MaxGroups+1 || counts[OtherGroup] != 50 {
		t.Fatalf("%d groups, (other)=%d; want %d groups and 50 folded", len(counts), counts[OtherGroup], MaxGroups+1)
	}
}

// Running out of budget stops the search at a correct prefix: nothing out of
// order, nothing skipped before the stop, and the cursor continues from it.
func TestSearch_ScanBudgetEndsAtACorrectPrefix(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	for i := 0; i < 20; i++ { // several days: blocks that do not overlap in time
		var batch []wire.Log
		for j := 0; j < 12; j++ {
			batch = append(batch, mkLog(base-int64(i)*3_600_000+int64(j), "api", "info", fmt.Sprintf("m%02d.%02d", i, j), 0))
		}
		_ = s.Append(context.Background(), batch)
		_ = s.Flush()
	}
	full := jsons(all(t, s, "", wideFrom, wideTo, SearchOpts{Limit: 1000}))
	if len(full) != 240 {
		t.Fatalf("%d logs", len(full))
	}
	// A budget of a few blocks.
	res, err := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, SearchOpts{Limit: 1000, ScanBudget: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Cursor == "" {
		t.Fatalf("truncated=%v cursor=%q; want a truncated search that can continue", res.Truncated, res.Cursor)
	}
	got := jsons(res.Logs)
	if len(got) == 0 || len(got) >= len(full) || strings.Join(got, "\n") != strings.Join(full[:len(got)], "\n") {
		t.Fatalf("a budgeted search returned %d logs that are not the first %d of the full result", len(got), len(got))
	}
	// Continuing from the cursor, again with a small budget, eventually yields everything.
	acc := append([]string(nil), got...)
	opts := SearchOpts{Limit: 1000, ScanBudget: 6000, Cursor: res.Cursor}
	for i := 0; i < 100 && len(acc) < len(full); i++ {
		r, err := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, opts)
		if err != nil {
			t.Fatal(err)
		}
		acc = append(acc, jsons(r.Logs)...)
		if r.Cursor == "" {
			break
		}
		opts.Cursor = r.Cursor
	}
	if strings.Join(acc, "\n") != strings.Join(full, "\n") {
		t.Fatalf("budgeted pages added up to %d logs, want the full %d in the same order", len(acc), len(full))
	}
}

func TestSearch_RejectsAMalformedCursor(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	for _, c := range []string{"!!!", "bm90LWEtY3Vyc29y", "MTIz"} {
		if _, err := s.Search(context.Background(), nil, 0, 10, SearchOpts{Cursor: c}); err == nil {
			t.Errorf("cursor %q was accepted", c)
		}
	}
}

// L6: writers, searchers and flushers at once, under -race. Every log is
// appended exactly once and every one must be there at the end, exactly once.
func TestStore_ConcurrentWritersSearchersAndFlushers(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	const writers, per = 6, 120
	var wg sync.WaitGroup
	var stop atomic.Bool
	base := t0.UnixMilli()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				svc := pSvc[(w+i)%len(pSvc)]
				l := mkLog(base+int64(i*7+w), svc, pStatus[i%len(pStatus)], fmt.Sprintf("w%d-%03d", w, i), i)
				if err := s.Append(context.Background(), []wire.Log{l}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	var readers sync.WaitGroup
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !stop.Load() {
				res, err := s.Search(context.Background(), mustParse(t, "service:api OR status:error"), wideFrom, wideTo, SearchOpts{Limit: 50})
				if err != nil {
					t.Error(err)
					return
				}
				seen := map[string]bool{}
				for _, l := range res.Logs {
					if seen[l.Message] {
						t.Errorf("a page repeated %q", l.Message)
					}
					seen[l.Message] = true
				}
			}
		}()
	}
	readers.Add(1)
	go func() {
		defer readers.Done()
		for !stop.Load() {
			_ = s.Flush()
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	stop.Store(true)
	readers.Wait()
	got := everything(t, s)
	if len(got) != writers*per {
		t.Fatalf("%d logs, want %d", len(got), writers*per)
	}
	seen := map[string]int{}
	for _, m := range got {
		seen[m]++
	}
	var dup []string
	for m, n := range seen {
		if n != 1 {
			dup = append(dup, m)
		}
	}
	sort.Strings(dup)
	if len(dup) > 0 {
		t.Fatalf("logs appearing other than once: %v", dup)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, d := range tempDirs {
		_ = os.RemoveAll(d)
	}
	os.Exit(code)
}
