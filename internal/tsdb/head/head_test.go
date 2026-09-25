package head

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// mustSelect is [Head.Select] for a test that has no reason to expect a
// decode error. Select reports one rather than returning a short read, so
// every call site has to say which it is.
func mustSelect(t interface {
	Helper()
	Fatalf(string, ...any)
}, h *Head, sel tsdb.Selector, from, to int64) []tsdb.SeriesSamples {
	t.Helper()
	got, err := h.Select(sel, from, to)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	return got
}

func ref(metric string, tags ...string) tsdb.SeriesRef {
	return tsdb.NewSeriesRef(metric, tags)
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// appendOne is the common case: one sample for one series.
func appendOne(h *Head, r tsdb.SeriesRef, t int64, v float64) error {
	_, rejected := h.Append([]tsdb.SeriesRef{r}, []Sample{{T: t, V: v}})
	if len(rejected) > 0 {
		return rejected[0].Err
	}
	return nil
}

// TestHead_SelectReportsACorruptChunkInsteadOfAShortRead is the regression
// test for a silent data loss with two durable copies. samplesIn used to stop
// at a chunk that failed to decode and return the samples it had, which the
// caller cannot tell from a series that is genuinely that short — and the
// caller is the block cut, which writes a block from exactly this snapshot
// and then truncates the head and the write-ahead log to match. One decode
// error dropped that chunk and every later chunk of the series from the
// block and from both copies of the log.
func TestHead_SelectReportsACorruptChunkInsteadOfAShortRead(t *testing.T) {
	h := New(Options{BlockRange: 1000})
	r := ref("m", "env:prod")
	// Two chunks: BlockRange is 1000ms, and a chunk never spans a boundary.
	for i := int64(0); i < 20; i++ {
		if err := appendOne(h, r, i*100, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	id := h.Postings().Select(tsdb.Selector{Metric: "m"})[0]
	if n := len(h.series(id).chunks); n < 2 {
		t.Fatalf("want at least two chunks, so the read has more to lose than the bad one: got %d", n)
	}
	if !h.DamageOneChunk(id) {
		t.Fatal("no chunk to damage")
	}

	got, err := h.Select(tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if err == nil {
		n := 0
		for _, s := range got {
			n += len(s.Samples)
		}
		t.Fatalf("Select returned %d samples and no error; a short read here is lost data", n)
	}
}

func TestHead_AppendAndSelect(t *testing.T) {
	h := New(Options{BlockRange: 1000})
	a := ref("http.request.count", "env:prod", "route:/api")
	b := ref("http.request.count", "env:dev", "route:/api")

	for i := int64(0); i < 5; i++ {
		if err := appendOne(h, a, i*100, float64(i)); err != nil {
			t.Fatal(err)
		}
		if err := appendOne(h, b, i*100, float64(i*10)); err != nil {
			t.Fatal(err)
		}
	}
	got := mustSelect(t, h, tsdb.Selector{Metric: "http.request.count"}, 0, 1000)
	if len(got) != 2 {
		t.Fatalf("selected %d series, want 2", len(got))
	}
	// Sorted by series key, so results are deterministic.
	if got[0].Series.Key() > got[1].Series.Key() {
		t.Error("results are not sorted by series key")
	}
	if n := len(got[0].Samples); n != 5 {
		t.Errorf("series 0 has %d samples, want 5", n)
	}
	// Tag selection goes through the index.
	only := mustSelect(t, h, tsdb.Selector{
		Metric:   "http.request.count",
		Matchers: []tsdb.Matcher{{Key: "env", Value: "prod", Type: tsdb.Equal}},
	}, 0, 1000)
	if len(only) != 1 || only[0].Series.Key() != a.Key() {
		t.Errorf("tag selection returned %d series", len(only))
	}
	// Time bounds are inclusive and exclude what falls outside.
	window := mustSelect(t, h, tsdb.Selector{Metric: "http.request.count"}, 100, 200)
	if n := len(window[0].Samples); n != 2 {
		t.Errorf("window returned %d samples, want 2", n)
	}
}

func TestHead_OrderingRules(t *testing.T) {
	h := New(Options{BlockRange: 1_000_000})
	r := ref("m", "env:prod")
	if err := appendOne(h, r, 1000, 1); err != nil {
		t.Fatal(err)
	}

	t.Run("out of order is rejected", func(t *testing.T) {
		if err := appendOne(h, r, 999, 2); !errors.Is(err, ErrOutOfOrder) {
			t.Errorf("err = %v, want ErrOutOfOrder", err)
		}
	})
	t.Run("a repeat of the newest sample is idempotent", func(t *testing.T) {
		// An agent retry resends what it already sent; accepting it as a
		// no-op is what makes at-least-once delivery safe.
		if err := appendOne(h, r, 1000, 1); err != nil {
			t.Errorf("duplicate (t,v) should be accepted as a no-op, got %v", err)
		}
		got := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, 2000)
		if n := len(got[0].Samples); n != 1 {
			t.Errorf("duplicate created %d samples, want 1", n)
		}
	})
	t.Run("same timestamp with a different value is a conflict", func(t *testing.T) {
		if err := appendOne(h, r, 1000, 99); !errors.Is(err, ErrOutOfOrder) {
			t.Errorf("err = %v, want ErrOutOfOrder", err)
		}
	})
	t.Run("out of bounds after a truncation", func(t *testing.T) {
		h.Truncate(5000)
		if err := appendOne(h, r, 4000, 1); !errors.Is(err, ErrOutOfBounds) {
			t.Errorf("err = %v, want ErrOutOfBounds", err)
		}
		if err := appendOne(h, r, 6000, 1); err != nil {
			t.Errorf("a sample after minValidTime should be accepted: %v", err)
		}
	})
}

func TestHead_CutsChunksAtTheCapAndAtBlockBoundaries(t *testing.T) {
	const blockRange = 1000
	h := New(Options{BlockRange: blockRange})
	r := ref("m")

	// 200 samples one ms apart: more than a chunk holds, all inside one block.
	for i := int64(0); i < 200; i++ {
		if err := appendOne(h, r, i, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if st := h.Stats(); st.Chunks != 2 {
		t.Errorf("after 200 samples there are %d chunks, want 2 (cut at 120)", st.Chunks)
	}
	// Crossing a block boundary must start a new chunk even though the
	// current one has room — otherwise cutting a block would have to split it.
	if err := appendOne(h, r, blockRange+1, 1); err != nil {
		t.Fatal(err)
	}
	if st := h.Stats(); st.Chunks != 3 {
		t.Errorf("crossing a block boundary gave %d chunks, want 3", st.Chunks)
	}
	got := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if n := len(got[0].Samples); n != 201 {
		t.Errorf("selected %d samples across chunks, want 201", n)
	}
}

func TestHead_SeriesLimitIsPerMetric(t *testing.T) {
	h := New(Options{MaxSeriesPerMetric: 3})
	for i := 0; i < 3; i++ {
		if err := appendOne(h, ref("m", fmt.Sprintf("host:h%d", i)), 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	err := appendOne(h, ref("m", "host:one-too-many"), 1, 1)
	if !errors.Is(err, ErrSeriesLimit) {
		t.Errorf("err = %v, want ErrSeriesLimit", err)
	}
	// Another metric has its own budget — the limit protects against one
	// runaway metric, not against the database having many metrics.
	if err := appendOne(h, ref("other", "host:x"), 1, 1); err != nil {
		t.Errorf("a different metric should not be limited: %v", err)
	}
	if st := h.Stats(); st.LimitRejects != 1 {
		t.Errorf("LimitRejects = %d, want 1", st.LimitRejects)
	}
}

func TestHead_OneBadSeriesDoesNotDiscardTheBatch(t *testing.T) {
	h := New(Options{MaxSeriesPerMetric: 1})
	if err := appendOne(h, ref("m", "host:a"), 1, 1); err != nil {
		t.Fatal(err)
	}
	refs := []tsdb.SeriesRef{
		ref("m", "host:a"),        // fine
		ref("m", "host:rejected"), // over the limit
		ref("other", "host:c"),    // fine
	}
	samples := []Sample{{T: 2, V: 1}, {T: 2, V: 2}, {T: 2, V: 3}}
	stored, rejected := h.Append(refs, samples)
	if n := countTrue(stored); n != 2 {
		t.Errorf("stored %d, want 2", n)
	}
	if len(rejected) != 1 || !errors.Is(rejected[0].Err, ErrSeriesLimit) {
		t.Fatalf("rejected = %v, want one ErrSeriesLimit", rejected)
	}
	// The index is what lets a caller attribute the refusal without parsing
	// an error message.
	if rejected[0].Index != 1 {
		t.Errorf("rejection points at entry %d, want 1", rejected[0].Index)
	}
}

func TestHead_TruncateDropsOldChunksAndForgetsEmptySeries(t *testing.T) {
	h := New(Options{BlockRange: 100})
	stale := ref("m", "host:stale")
	live := ref("m", "host:live")
	for i := int64(0); i < 5; i++ {
		if err := appendOne(h, stale, i*100, 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(0); i < 10; i++ {
		if err := appendOne(h, live, i*100, 1); err != nil {
			t.Fatal(err)
		}
	}
	series, chunks := h.Truncate(500)
	if series != 1 {
		t.Errorf("dropped %d series, want 1 (the one with nothing left)", series)
	}
	if chunks == 0 {
		t.Error("expected some chunks to be dropped")
	}
	// The forgotten series must be gone from the index too, or it would still
	// match queries and leak memory.
	if got := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64); len(got) != 1 {
		t.Errorf("after truncation %d series are selectable, want 1", len(got))
	}
	if got := h.Postings().Values("m", "host"); len(got) != 1 || got[0] != "live" {
		t.Errorf("postings still hold %v", got)
	}
	// And its slot is reusable: appending it again is a fresh series.
	if err := appendOne(h, stale, 900, 1); err != nil {
		t.Errorf("re-appending a forgotten series failed: %v", err)
	}
}

func TestHead_ConcurrentAppendsAreCountedExactly(t *testing.T) {
	h := New(Options{BlockRange: 1 << 40})
	const writers, perWriter = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := ref("m", fmt.Sprintf("writer:%d", w))
			for i := 0; i < perWriter; i++ {
				if err := appendOne(h, r, int64(i), float64(i)); err != nil {
					t.Errorf("writer %d sample %d: %v", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	st := h.Stats()
	if st.Series != writers {
		t.Errorf("Series = %d, want %d", st.Series, writers)
	}
	if want := int64(writers * perWriter); st.Samples != want {
		t.Errorf("Samples = %d, want %d", st.Samples, want)
	}
}

// --- durability ------------------------------------------------------------

// failingLog stands in for a full disk: Log works n times, then never again.
type failingLog struct {
	ok     int
	synced int
}

func (l *failingLog) Log(...wal.Record) error {
	if l.ok <= 0 {
		return errors.New("disk full")
	}
	l.ok--
	return nil
}
func (l *failingLog) Sync() error { l.synced++; return nil }

func TestHead_AWALFailureKeepsSamplesInvisible(t *testing.T) {
	// The whole point of writing ahead: if the record did not make it, the
	// sample must not be readable, or a crash would lose data a query already
	// returned.
	lg := &failingLog{ok: 1}
	h := New(Options{WAL: lg, BlockRange: 1 << 40, SyncOnAppend: true})
	r := ref("m", "host:a")
	if err := appendOne(h, r, 1, 1); err != nil {
		t.Fatal(err)
	}
	if lg.synced != 1 {
		t.Errorf("SyncOnAppend synced %d times, want 1", lg.synced)
	}
	stored, rejected := h.Append([]tsdb.SeriesRef{r}, []Sample{{T: 2, V: 2}})
	if countTrue(stored) != 0 || len(rejected) == 0 {
		t.Fatalf("stored %v with %v; a failed log must store nothing", stored, rejected)
	}
	// A log failure is not about one series, so it carries no index.
	if rejected[len(rejected)-1].Index != -1 {
		t.Errorf("a log failure was attributed to entry %d", rejected[len(rejected)-1].Index)
	}
	got := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if n := len(got[0].Samples); n != 1 {
		t.Errorf("%d samples are visible, want 1 (the one that was logged)", n)
	}
}

func TestHead_AppendRejectsAMalformedBatch(t *testing.T) {
	h := New(Options{})
	_, rejected := h.Append([]tsdb.SeriesRef{ref("m")}, nil)
	if len(rejected) != 1 || rejected[0].Index != -1 {
		t.Errorf("rejected = %v, want one unattributed error for a length mismatch", rejected)
	}
	if h.MinValidTime() != math.MinInt64 {
		t.Errorf("a fresh head has MinValidTime %d, want the minimum", h.MinValidTime())
	}
	h.Truncate(500)
	if h.MinValidTime() != 500 {
		t.Errorf("MinValidTime = %d after truncating to 500", h.MinValidTime())
	}
}

func TestKeepForCheckpoint(t *testing.T) {
	keep := KeepForCheckpoint(1000)

	// Series records always survive: a sample record names its series by an id
	// that only a series record defines.
	if !keep(wal.Record{Type: RecordSeries}) {
		t.Error("series records must be kept")
	}
	if keep(wal.Record{Type: 200}) {
		t.Error("an unknown record type is not something to carry forward")
	}

	samples := func(ts ...int64) wal.Record {
		batch := make([]Sample, len(ts))
		for i, t := range ts {
			batch[i] = Sample{ID: 1, T: t, V: float64(t)}
		}
		return wal.Record{Type: RecordSamples, Data: encodeSamples(nil, batch)}
	}
	// Entirely below the head's window: a block holds these now.
	if keep(samples(1, 500, 999)) {
		t.Error("a record whose samples are all in a block must not be carried forward")
	}
	// The case the old policy got wrong. These samples are in the head and
	// nowhere else on disk, so dropping the segment loses them outright —
	// which is exactly what happened once the log rolled a segment.
	if !keep(samples(1000, 1500)) {
		t.Error("a record still holding live samples must be kept")
	}
	if !keep(samples(1_000_000)) {
		t.Error("a record ahead of the window must be kept")
	}
	// Straddling: kept whole, dead samples and all. Replay refuses those by
	// the same bounds check that refuses anything a block already covers.
	if !keep(samples(1, 2000)) {
		t.Error("a record straddling the boundary must be kept")
	}

	// Undecodable but correctly framed: keep the bytes and let replay judge.
	if !keep(wal.Record{Type: RecordSamples, Data: []byte{0xff}}) {
		t.Error("an undecodable sample record must be kept, not silently dropped")
	}
}

func TestHead_ReplayRebuildsTheHead(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{WAL: w, BlockRange: 1000, SyncOnAppend: true})

	refs := []tsdb.SeriesRef{ref("m", "host:a"), ref("m", "host:b")}
	for i := int64(0); i < 50; i++ {
		for _, r := range refs {
			if err := appendOne(h, r, i*10, float64(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	wantIDs := map[string]uint64{}
	for _, id := range h.Postings().Select(tsdb.Selector{Metric: "m"}) {
		r, _ := h.Series(id)
		wantIDs[r.Key()] = id
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process: nothing but the log on disk.
	restored := New(Options{BlockRange: 1000})
	st, err := Replay(restored, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples != 100 {
		t.Errorf("replayed %d samples, want 100", st.Samples)
	}
	got := mustSelect(t, restored, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if len(got) != len(want) {
		t.Fatalf("replayed %d series, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Series.Key() != want[i].Series.Key() {
			t.Errorf("series %d is %s, want %s", i, got[i].Series.Key(), want[i].Series.Key())
		}
		if len(got[i].Samples) != len(want[i].Samples) {
			t.Errorf("series %s has %d samples, want %d",
				got[i].Series.Key(), len(got[i].Samples), len(want[i].Samples))
			continue
		}
		for j := range want[i].Samples {
			if got[i].Samples[j] != want[i].Samples[j] {
				t.Errorf("series %s sample %d = %v, want %v",
					got[i].Series.Key(), j, got[i].Samples[j], want[i].Samples[j])
			}
		}
	}
	// Ids must survive, or a sample record would point at the wrong series.
	for _, id := range restored.Postings().Select(tsdb.Selector{Metric: "m"}) {
		r, _ := restored.Series(id)
		if wantIDs[r.Key()] != id {
			t.Errorf("series %s came back as id %d, want %d", r.Key(), id, wantIDs[r.Key()])
		}
	}
	// And a new series gets an id above everything the log used.
	if err := appendOne(restored, ref("m", "host:new"), 10_000, 1); err != nil {
		t.Fatal(err)
	}
}

func TestHead_ReplayMatchesTheHeadUnderConcurrentWriters(t *testing.T) {
	// The invariant Append's logMu buys: WAL order is apply order, so replay
	// applies exactly the samples the head accepted. Racing writers on one
	// series exercise it — whoever loses the race is rejected identically by
	// the head and by replay.
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{WAL: w, BlockRange: 1 << 40})

	shared := ref("m", "host:shared")
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				// Deliberately overlapping timestamps: most of these lose.
				_, _ = h.Append([]tsdb.SeriesRef{shared}, []Sample{{T: int64(i), V: float64(g)}})
			}
		}(g)
	}
	wg.Wait()
	want := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	restored := New(Options{BlockRange: 1 << 40})
	if _, err := Replay(restored, dir); err != nil {
		t.Fatal(err)
	}
	got := mustSelect(t, restored, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("got %d series, head had %d", len(got), len(want))
	}
	if len(got[0].Samples) != len(want[0].Samples) {
		t.Fatalf("replay has %d samples, head had %d", len(got[0].Samples), len(want[0].Samples))
	}
	for i := range want[0].Samples {
		if got[0].Samples[i] != want[0].Samples[i] {
			t.Fatalf("sample %d is %+v, head had %+v", i, got[0].Samples[i], want[0].Samples[i])
		}
	}
}

func TestHead_ReplayAfterATornTailKeepsWhatWasAcknowledged(t *testing.T) {
	// The crash case: the process died mid-write. Everything synced before
	// that must come back, and the partial record must not stop replay.
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{WAL: w, BlockRange: 1 << 40, SyncOnAppend: true})
	r := ref("m", "host:a")
	for i := int64(0); i < 20; i++ {
		if err := appendOne(h, r, i, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	truncateLastSegment(t, dir, 3)

	restored := New(Options{BlockRange: 1 << 40})
	st, err := Replay(restored, dir)
	if err != nil {
		t.Fatalf("a torn tail must not fail replay: %v", err)
	}
	if st.Samples == 0 || st.Samples > 20 {
		t.Errorf("replayed %d samples, want between 1 and 20", st.Samples)
	}
	got := mustSelect(t, restored, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if len(got) != 1 {
		t.Fatalf("replayed %d series", len(got))
	}
	// Whatever survived must be a prefix: no gaps, no reordering.
	for i, s := range got[0].Samples {
		if s.T != int64(i) {
			t.Fatalf("sample %d has t=%d; replay is not a prefix of what was written", i, s.T)
		}
	}
}

func TestHead_ReplayRejectsTheSamplesTheHeadWouldReject(t *testing.T) {
	// Replay is not a second chance: a log record that violates the ordering
	// rules is skipped and counted, not applied.
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	r := ref("m", "host:a")
	buf := encodeSeries(nil, 1, r)
	if err := w.Log(wal.Record{Type: RecordSeries, Data: buf}); err != nil {
		t.Fatal(err)
	}
	samples := []Sample{{ID: 1, T: 100, V: 1}, {ID: 1, T: 50, V: 2}, {ID: 99, T: 200, V: 3}}
	if err := w.Log(wal.Record{Type: RecordSamples, Data: encodeSamples(nil, samples)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h := New(Options{BlockRange: 1 << 40})
	st, err := Replay(h, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples != 1 {
		t.Errorf("applied %d samples, want 1", st.Samples)
	}
	if st.OOORejected != 2 {
		t.Errorf("skipped %d samples, want 2 (one out of order, one unknown series)", st.OOORejected)
	}
}

func TestHead_ReplayIgnoresUnknownRecordTypes(t *testing.T) {
	// Forward compatibility: a newer ozyd may write records this one
	// does not know. Skipping them is why the format has a type byte.
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(
		wal.Record{Type: RecordSeries, Data: encodeSeries(nil, 1, ref("m"))},
		wal.Record{Type: 99, Data: []byte("from the future")},
		wal.Record{Type: RecordSamples, Data: encodeSamples(nil, []Sample{{ID: 1, T: 5, V: 1}})},
	); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h := New(Options{BlockRange: 1 << 40})
	st, err := Replay(h, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples != 1 {
		t.Errorf("applied %d samples, want 1", st.Samples)
	}
}

func TestHead_ReplayReportsCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// A series record whose tag count is absurd: the CRC is valid (we encode
	// it ourselves), so only the decoder can catch it.
	bad := append([]byte{}, encodeSeries(nil, 1, ref("m"))...)
	bad[len(bad)-1] = 0x7f
	if err := w.Log(wal.Record{Type: RecordSeries, Data: bad}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(New(Options{}), dir); err == nil {
		t.Error("a malformed series record should fail replay")
	}
}

func TestRecords_RoundTrip(t *testing.T) {
	r := ref("http.request.count", "env:prod", "route:/api/comics", "status:200")
	id, got, err := decodeSeries(encodeSeries(nil, 4242, r))
	if err != nil {
		t.Fatal(err)
	}
	if id != 4242 || got.Key() != r.Key() {
		t.Errorf("series round trip: id=%d key=%s", id, got.Key())
	}
	// A series with no tags, and one with an empty tag value.
	for _, r := range []tsdb.SeriesRef{ref("m"), {Metric: "m", Tags: []tsdb.Tag{{Key: "k"}}}} {
		_, got, err := decodeSeries(encodeSeries(nil, 1, r))
		if err != nil {
			t.Fatalf("%v: %v", r, err)
		}
		if got.Key() != r.Key() {
			t.Errorf("round trip gave %s, want %s", got.Key(), r.Key())
		}
	}
	want := []Sample{{ID: 1, T: -5, V: math.Inf(-1)}, {ID: 2, T: 1 << 40, V: math.NaN()}}
	gotSamples, err := decodeSamples(encodeSamples(nil, want), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSamples) != 2 || gotSamples[0].T != -5 || !math.IsNaN(gotSamples[1].V) {
		t.Errorf("samples round trip: %+v", gotSamples)
	}
}

func TestRecords_TruncatedInputIsAnError(t *testing.T) {
	full := encodeSeries(nil, 1, ref("m", "env:prod"))
	for i := 1; i < len(full); i++ {
		if _, _, err := decodeSeries(full[:i]); err == nil {
			t.Errorf("decodeSeries accepted %d of %d bytes", i, len(full))
		}
	}
	samples := encodeSamples(nil, []Sample{{ID: 1, T: 1, V: 1}})
	for i := 1; i < len(samples); i++ {
		if _, err := decodeSamples(samples[:i], nil); err == nil {
			t.Errorf("decodeSamples accepted %d of %d bytes", i, len(samples))
		}
	}
}

func truncateLastSegment(t *testing.T, dir string, cut int64) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.wal"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no segments in %s: %v", dir, err)
	}
	last := matches[len(matches)-1]
	info, err := os.Stat(last)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(last, info.Size()-cut); err != nil {
		t.Fatal(err)
	}
}

// flakyLogger fails the first n Log calls and then behaves.
type flakyLogger struct {
	inner Logger
	fail  int
}

func (f *flakyLogger) Log(recs ...wal.Record) error {
	if f.fail > 0 {
		f.fail--
		return errors.New("disk on fire")
	}
	return f.inner.Log(recs...)
}

func (f *flakyLogger) Sync() error { return f.inner.Sync() }

func TestHead_ASeriesRecordIsWrittenUntilItLands(t *testing.T) {
	// The deterministic half of the bug that
	// TestHead_ReplayMatchesTheHeadUnderConcurrentWriters catches by racing:
	// a series exists in memory but its definition never reached the log.
	// Here the first log write fails, so the series is in the head with
	// nothing on disk to say what id 1 means. The next append has to declare
	// it again — if "new series" meant "the caller who created it", that
	// caller is gone and the sample is orphaned forever.
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{WAL: &flakyLogger{inner: w, fail: 1}, BlockRange: 1 << 40})

	r := ref("m", "host:a")
	if err := appendOne(h, r, 1, 10); err == nil {
		t.Fatal("append should have failed: the log write failed")
	}
	if err := appendOne(h, r, 2, 20); err != nil {
		t.Fatalf("second append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	restored := New(Options{BlockRange: 1 << 40})
	st, err := Replay(restored, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples != 1 {
		t.Fatalf("replayed %d samples, want 1 (the one the log accepted)", st.Samples)
	}
	got := mustSelect(t, restored, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if len(got) != 1 || len(got[0].Samples) != 1 || got[0].Samples[0].V != 20 {
		t.Fatalf("restored %+v, want the single sample at t=2", got)
	}
	// And the failed batch left nothing behind: t=1 was never visible.
	if head := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64); len(head[0].Samples) != 1 {
		t.Fatalf("head holds %+v, want only the sample that was logged", head[0].Samples)
	}
}
