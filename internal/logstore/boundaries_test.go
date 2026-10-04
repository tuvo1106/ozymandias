package logstore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// A stream that moves on to another day closes the earlier day's chunk with a
// footer, so a closed day is complete and readable without walking its blocks.
func TestStore_AClosedDaysChunkIsSealedWithAFooter(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(t, dir)
	defer func() { _ = s.Close() }()
	day1, day2 := t0.Add(-48*time.Hour).UnixMilli(), t0.UnixMilli()
	_ = s.Append(context.Background(), batchOf("a", 0, 3, day1))
	_ = s.Flush()
	_ = s.Append(context.Background(), batchOf("b", 0, 3, day2))
	_ = s.Flush()
	var sealed, open int
	for _, c := range findFiles(t, dir, ".chunk") {
		data, _ := os.ReadFile(c)
		ix, err := readIndex(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if ix.Sealed {
			sealed++
		} else {
			open++
		}
	}
	if sealed != 1 || open != 1 {
		t.Fatalf("%d sealed and %d open chunks; want the earlier day closed (footer written) and the current one still open", sealed, open)
	}
}

// Sequence numbers must keep rising across a restart even when the WAL no
// longer holds the old ones: a new entry that reused a sealed number would be
// dropped as "already sealed" at the next recovery.
func TestStore_SequenceNumbersSurviveARestartWithAnEmptyWAL(t *testing.T) {
	dir := t.TempDir()
	s := openSynced(t, dir)
	base := t0.UnixMilli()
	_ = s.Append(context.Background(), batchOf("old", 0, 20, base))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "wal")); err != nil {
		t.Fatal(err)
	}
	// Blocks big enough that the new entries stay in the WAL: the case where recovery
	// has to tell them from the sealed old ones by number alone.
	keepInWAL := func(o *Options) { o.NoSync = false; o.BlockBytes = 1 << 30 }
	r, _ := openStore(t, dir, keepInWAL)
	defer func() { _ = r.Close() }()
	_ = r.Append(context.Background(), batchOf("new", 0, 20, base))
	again, _ := openStore(t, crashCopy(t, dir), keepInWAL) // crash with the new entries only in the WAL
	defer func() { _ = again.Close() }()
	got := everything(t, again)
	want := append(wantMessages("old", 20), wantMessages("new", 20)...)
	if len(got) != len(want) {
		t.Fatalf("%d logs after restart + crash, want %d: new entries were taken for already-sealed ones", len(got), len(want))
	}
}

// A page never holds more than the limit, and it carries a cursor exactly when
// there is more to read.
func TestSearch_PageSizeAndCursorAgreeWithTheLimit(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	_ = s.Append(context.Background(), batchOf("m", 0, 10, base))
	_ = s.Flush()
	_ = s.Append(context.Background(), batchOf("m", 10, 5, base))
	for _, tc := range []struct {
		limit      int
		wantLogs   int
		wantCursor bool
	}{{1, 1, true}, {7, 7, true}, {14, 14, true}, {15, 15, false}, {16, 15, false}, {1000, 15, false}} {
		res, err := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, SearchOpts{Limit: tc.limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Logs) != tc.wantLogs || (res.Cursor != "") != tc.wantCursor {
			t.Errorf("limit %d: %d logs, cursor=%v; want %d logs, cursor=%v", tc.limit, len(res.Logs), res.Cursor != "", tc.wantLogs, tc.wantCursor)
		}
	}
}

// The scan budget is spent in whole blocks: a budget of exactly what a search
// needs is enough, one byte less is not.
func TestSearch_ScanBudgetBoundaryIsExact(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	for i := 0; i < 5; i++ {
		_ = s.Append(context.Background(), batchOf(fmt.Sprintf("b%d-", i), 0, 8, base-int64(i)*3_600_000))
		_ = s.Flush()
	}
	full, err := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, SearchOpts{Limit: 1000})
	if err != nil || full.Truncated || full.Stats.BytesRead == 0 {
		t.Fatalf("setup: %+v %v", full, err)
	}
	exact, _ := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, SearchOpts{Limit: 1000, ScanBudget: full.Stats.BytesRead})
	if exact.Truncated || len(exact.Logs) != 40 {
		t.Errorf("a budget of exactly %d bytes: truncated=%v, %d logs", full.Stats.BytesRead, exact.Truncated, len(exact.Logs))
	}
	short, _ := s.Search(context.Background(), mustParse(t, ""), wideFrom, wideTo, SearchOpts{Limit: 1000, ScanBudget: full.Stats.BytesRead - 1})
	if !short.Truncated {
		t.Error("a budget one byte short was not reported as truncated")
	}
}

func TestMod_RoundsDownForNegativeTimestamps(t *testing.T) {
	for _, c := range []struct{ a, b, want int64 }{{7, 5, 2}, {-1, 1000, 999}, {-1000, 1000, 0}, {0, 7, 0}, {-3, 2, 1}} {
		if got := mod(c.a, c.b); got != c.want {
			t.Errorf("mod(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFacets_LimitDuplicatesAndTheCap(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	var batch []wire.Log
	add := func(n int, status string, tags []any) {
		for i := 0; i < n; i++ {
			l := mkLog(base, "api", status, "m", 0)
			l.Attrs["tags"] = tags
			batch = append(batch, l)
		}
	}
	add(5, "error", []any{"a", "a", "b"}) // "a" twice in one log counts once
	add(3, "warn", []any{"b"})
	add(1, "info", nil)
	_ = s.Append(context.Background(), batch)
	res, err := s.Facets(context.Background(), nil, base-1, base+1, []string{"status", "@tags"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Facets["status"]; len(got) != 2 || got[0] != (FacetCount{"error", 5}) || got[1] != (FacetCount{"warn", 3}) {
		t.Errorf("status facet with limit 2 = %v", got)
	}
	if got := res.Facets["@tags"]; len(got) != 2 || got[0] != (FacetCount{"b", 8}) || got[1] != (FacetCount{"a", 5}) {
		t.Errorf("@tags facet = %v; a log listing a value twice counts it once", got)
	}

	// Past MaxFacetValues distinct values the facet stops tracking new ones and says so.
	var many []wire.Log
	for i := 0; i < MaxFacetValues+50; i++ {
		l := mkLog(base+1, "web", "info", "m", 0)
		l.Attrs["user"] = fmt.Sprintf("u%05d", i)
		many = append(many, l)
	}
	_ = s.Append(context.Background(), many)
	res, err = s.Facets(context.Background(), mustParse(t, "service:web"), base, base+2, []string{"@user"}, MaxFacetValues+100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facets["@user"]) != MaxFacetValues || len(res.Capped) != 1 || res.Capped[0] != "@user" {
		t.Errorf("%d values tracked, capped=%v; want exactly %d and the key reported", len(res.Facets["@user"]), res.Capped, MaxFacetValues)
	}
}

// Retention counts from the END of a day: a day that is still partly inside
// the window is kept, whole.
func TestStore_RetentionCountsFromTheEndOfTheDay(t *testing.T) {
	logDay := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		now  time.Time
		gone bool
	}{
		{"cutoff in the middle of the day", time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), false},
		{"cutoff just before the day ends", time.Date(2026, 10, 3, 23, 59, 0, 0, time.UTC), false},
		{"cutoff at the end of the day", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), true},
	} {
		dir := t.TempDir()
		s, clk := openStoreAt(t, dir, logDay, func(o *Options) { o.Retention = 48 * time.Hour })
		_ = s.Append(context.Background(), batchOf("m", 0, 3, logDay.UnixMilli()))
		_ = s.Flush()
		waitTicker(t, clk)
		clk.Set(tc.now)
		if tc.gone {
			waitFor(t, func() bool { return len(findFiles(t, dir, ".chunk")) == 0 }, tc.name+": the day was not deleted")
		} else {
			time.Sleep(60 * time.Millisecond)
			if n := len(findFiles(t, dir, ".chunk")); n != 1 {
				t.Errorf("%s: %d chunks; a day still partly inside retention must be kept", tc.name, n)
			}
		}
		_ = s.Close()
	}
}

// A head is sealed when it reaches BlockBytes, not only when it passes it.
func TestStore_AHeadSealsExactlyAtTheThreshold(t *testing.T) {
	l := mkLog(t0.UnixMilli(), "api", "info", "one", 1)
	body, _ := encodeLog(&l)
	exact := len(body) + 16 // what one entry adds to headBytes
	headLen := func(s *Store) int {
		s.smu.RLock()
		defer s.smu.RUnlock()
		n := 0
		for _, st := range s.byID {
			n += len(st.head)
		}
		return n
	}
	for _, tc := range []struct {
		blockBytes int
		sealed     bool
	}{{exact, true}, {exact + 1, false}, {exact - 1, true}} {
		s, _ := openStore(t, t.TempDir(), func(o *Options) { o.BlockBytes = tc.blockBytes })
		_ = s.Append(context.Background(), []wire.Log{l})
		if got := headLen(s) == 0; got != tc.sealed {
			t.Errorf("BlockBytes %d with a %d-byte entry: sealed=%v, want %v", tc.blockBytes, exact, got, tc.sealed)
		}
		_ = s.Close()
	}
}
