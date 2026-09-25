package head

import (
	"math"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// bigBatch builds n samples of one series, ascending, for the split cases.
func bigBatch(n int) ([]tsdb.SeriesRef, []Sample) {
	r := ref("m", "host:a")
	refs := make([]tsdb.SeriesRef, n)
	samples := make([]Sample, n)
	for i := range samples {
		refs[i] = r
		samples[i] = Sample{T: int64(i), V: float64(i)}
	}
	return refs, samples
}

// TestHead_ALargeBatchIsSplitAcrossWALRecords.
//
// logLocked encoded a whole Append batch into a single WAL record. Past a
// point that record exceeds wal.MaxRecordSize, and wal.Log rejects an
// oversized record before writing any of it — so Append failed the *entire*
// batch with the Index -1 error, and every retry of the same body failed
// identically. Nothing was corrupted and nothing was lost; the client simply
// had a request it could never get past, which is its own kind of outage.
//
// It is reachable from intake: the decompressed body limit is 16 MiB and there
// is a cap on series per request but none on points per request, so one dense
// request lands above the line.
//
// The assertion is that the batch was *split*, not that a 16 MiB record was
// avoided. maxSamplesPerRecord is derived from the worst case a sample can
// encode to — ten-byte varints for both the id and the timestamp — while an
// ordinary sample takes about twelve bytes, so a batch that reaches the split
// point is nowhere near the byte limit. Building one that genuinely overflows
// means ~1.4 million samples and a few hundred megabytes under -race, to
// re-derive a bound the constant already states. Splitting at the boundary is
// the property; the constant is the argument that the boundary is safe.
func TestHead_ALargeBatchIsSplitAcrossWALRecords(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{WAL: w, BlockRange: math.MaxInt64 / 4})

	n := maxSamplesPerRecord + 1
	refs, samples := bigBatch(n)
	stored, rejected := h.Append(refs, samples)
	if len(rejected) != 0 {
		t.Fatalf("a %d-sample batch was rejected: %+v", n, rejected[0])
	}
	if got := countTrue(stored); got != n {
		t.Errorf("stored %d of %d samples", got, n)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	sampleRecs, biggest := 0, 0
	r, err := wal.NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		rec := r.Record()
		if len(rec.Data) > biggest {
			biggest = len(rec.Data)
		}
		if rec.Type == RecordSamples {
			sampleRecs++
		}
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if sampleRecs < 2 {
		t.Errorf("a batch of %d samples (%d over the split point) produced %d sample record(s); "+
			"it must be split", n, n-maxSamplesPerRecord, sampleRecs)
	}
	if biggest > wal.MaxRecordSize {
		t.Errorf("wrote a %d-byte record, over the %d limit", biggest, wal.MaxRecordSize)
	}
}

// TestHead_ASplitBatchReplaysAsOneBatch. Splitting is only safe if the log
// reads back as the same samples in the same order. The records go to wal.Log
// in a single call for exactly that reason: it writes them consecutively under
// its own lock, so no other appender's records can land between the halves.
func TestHead_ASplitBatchReplaysAsOneBatch(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{WAL: w, BlockRange: math.MaxInt64 / 4, SyncOnAppend: true})

	n := maxSamplesPerRecord + 5
	refs, samples := bigBatch(n)
	if _, rejected := h.Append(refs, samples); len(rejected) != 0 {
		t.Fatalf("rejected: %+v", rejected[0])
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	restored := New(Options{BlockRange: math.MaxInt64 / 4})
	st, err := Replay(restored, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples != int64(n) {
		t.Fatalf("replay applied %d samples, want %d (%d skipped)", st.Samples, n, st.OOORejected)
	}
	got := mustSelect(t, restored, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if len(got) != 1 {
		t.Fatalf("replay produced %d series, want 1", len(got))
	}
	last := got[0].Samples[len(got[0].Samples)-1]
	if last.T != int64(n-1) || last.V != float64(n-1) {
		t.Errorf("the last sample replayed as %+v, want T=%d V=%d", last, n-1, n-1)
	}
}
