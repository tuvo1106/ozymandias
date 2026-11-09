package queue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// TestProperty_ConsumedSequenceIsTheProducedSequence is M7's L2 property for
// the queue: for any interleaving of produce, poll, commit, crash-with-a-torn-
// tail and restart, every consumer reads exactly the acknowledged records, in
// order, resuming after each restart at its committed offset — no gap, no
// reordering, and no redelivery of anything it committed.
//
// The model is two integers per consumer (cursor, committed) over a list of
// acknowledged payloads, so every Poll's result is checked exactly, not just
// for being plausible. Segments are tiny so that rolls and retention happen
// constantly.
func TestProperty_ConsumedSequenceIsTheProducedSequence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		segSize := int64(rapid.IntRange(30, 200).Draw(rt, "segSize"))
		openTopic := func() *Topic {
			tp, err := Open(Options{Dir: dir, SyncInterval: -1, SegmentSize: segSize})
			if err != nil {
				rt.Fatalf("Open: %v", err)
			}
			return tp
		}
		tp := openTopic()
		defer func() { _ = tp.Close() }()

		names := []string{"a", "b"}
		type model struct{ cursor, committed int }
		models := map[string]*model{}
		consumers := map[string]*Consumer{}
		attach := func() {
			for _, n := range names {
				c, err := tp.Consumer(n)
				if err != nil {
					rt.Fatalf("Consumer(%s): %v", n, err)
				}
				consumers[n] = c
				if models[n] == nil {
					models[n] = &model{}
				}
				models[n].cursor = models[n].committed
			}
		}
		attach()
		var acked []string

		steps := rapid.IntRange(1, 60).Draw(rt, "steps")
		for i := range steps {
			switch rapid.IntRange(0, 9).Draw(rt, "op") {
			case 0, 1, 2, 3: // produce
				p := fmt.Sprintf("p%d-%s", i, rapid.StringN(0, 20, -1).Draw(rt, "payload"))
				off, err := tp.Produce(context.Background(), []byte(p))
				if err != nil {
					rt.Fatalf("Produce: %v", err)
				}
				if int(off) != len(acked) {
					rt.Fatalf("offset %d, want %d", off, len(acked))
				}
				acked = append(acked, p)
			case 4, 5, 6: // poll
				n := rapid.SampledFrom(names).Draw(rt, "consumer")
				m := models[n]
				if m.cursor == len(acked) {
					continue // Poll would block
				}
				max := rapid.IntRange(1, 8).Draw(rt, "max")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				recs, err := consumers[n].Poll(ctx, max)
				cancel()
				if err != nil {
					rt.Fatalf("Poll(%s): %v", n, err)
				}
				want := acked[m.cursor:min(len(acked), m.cursor+max)]
				if len(recs) != len(want) {
					rt.Fatalf("Poll(%s) at %d returned %d records, want %d", n, m.cursor, len(recs), len(want))
				}
				for j, r := range recs {
					if int(r.Offset) != m.cursor+j || string(r.Payload) != want[j] {
						rt.Fatalf("Poll(%s)[%d] = (%d, %q), want (%d, %q)",
							n, j, r.Offset, r.Payload, m.cursor+j, want[j])
					}
				}
				m.cursor += len(recs)
			case 7, 8: // commit somewhere in (committed, cursor]
				n := rapid.SampledFrom(names).Draw(rt, "consumer")
				m := models[n]
				if m.cursor == 0 {
					continue
				}
				off := rapid.IntRange(0, m.cursor-1).Draw(rt, "commit")
				if err := consumers[n].Commit(uint64(off)); err != nil {
					rt.Fatalf("Commit(%s, %d): %v", n, off, err)
				}
				m.committed = max(m.committed, off+1)
			case 9: // crash and restart, maybe leaving a torn record or a torn commit
				if err := tp.Close(); err != nil {
					rt.Fatalf("Close: %v", err)
				}
				if rapid.Bool().Draw(rt, "tornRecord") {
					rec := wal.Record{Type: recordType, Data: encodeRecord(uint64(len(acked)), time.Unix(0, 0), []byte("unacked"))}
					appendTornRT(rt, dir, rec, rapid.IntRange(1, wal.RecordOverhead+recordHeader+6).Draw(rt, "keep"))
				}
				if rapid.Bool().Draw(rt, "tornCommit") {
					p := filepath.Join(dir, consumersDir, rapid.SampledFrom(names).Draw(rt, "who")+offsetExt+".tmp")
					if err := os.WriteFile(p, []byte{9, 9, 9}, 0o644); err != nil {
						rt.Fatal(err)
					}
				}
				tp = openTopic()
				attach()
			}
		}
		// Drain both: everything acknowledged is still there.
		for _, n := range names {
			m := models[n]
			for m.cursor < len(acked) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				recs, err := consumers[n].Poll(ctx, 100)
				cancel()
				if err != nil {
					rt.Fatalf("drain Poll(%s): %v", n, err)
				}
				for j, r := range recs {
					if int(r.Offset) != m.cursor+j || string(r.Payload) != acked[m.cursor+j] {
						rt.Fatalf("drain(%s)[%d] = (%d, %q), want (%d, %q)",
							n, j, r.Offset, r.Payload, m.cursor+j, acked[m.cursor+j])
					}
				}
				m.cursor += len(recs)
			}
		}
	})
}

func appendTornRT(rt *rapid.T, dir string, rec wal.Record, keep int) {
	scratch, err := os.MkdirTemp("", "torn")
	if err != nil {
		rt.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	w, err := wal.Open(wal.Options{Dir: scratch})
	if err != nil {
		rt.Fatal(err)
	}
	if err := w.Log(rec); err != nil {
		rt.Fatal(err)
	}
	_ = w.Close()
	full, err := os.ReadFile(filepath.Join(scratch, wal.SegmentName(0)))
	if err != nil {
		rt.Fatal(err)
	}
	keep = min(keep, len(full)-1)
	segs, err := wal.Segments(dir)
	if err != nil {
		rt.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, wal.SegmentName(segs[len(segs)-1])), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		rt.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(full[:keep]); err != nil {
		rt.Fatal(err)
	}
}

// TestConcurrent_ProduceConsumeRetain is the L6 case: many producers, two
// consumers committing as they go, segments rolling and being retired under
// them, all under -race. Each consumer must see every offset exactly once and
// in order, with the payload its producer sent.
func TestConcurrent_ProduceConsumeRetain(t *testing.T) {
	testutil.CheckGoroutines(t)
	tp, err := Open(Options{Dir: t.TempDir(), SegmentSize: 512, SyncInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tp.Close() }()

	const producers, each = 8, 150
	total := producers * each
	var (
		mu   sync.Mutex
		sent = map[uint64]string{}
	)
	consumers := []string{"one", "two"}
	cs := map[string]*Consumer{}
	for _, n := range consumers {
		c, err := tp.Consumer(n)
		if err != nil {
			t.Fatal(err)
		}
		cs[n] = c
	}

	var wg sync.WaitGroup
	errs := make(chan error, producers+len(consumers))
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				payload := fmt.Sprintf("%d/%d", p, i)
				off, err := tp.Produce(context.Background(), []byte(payload))
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				sent[off] = payload
				mu.Unlock()
			}
		}()
	}
	got := map[string][]Record{}
	var gotMu sync.Mutex
	for _, n := range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := cs[n]
			var mine []Record
			for len(mine) < total {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				recs, err := c.Poll(ctx, 37)
				cancel()
				if err != nil {
					errs <- fmt.Errorf("%s: %w", n, err)
					return
				}
				mine = append(mine, recs...)
				if err := c.Commit(recs[len(recs)-1].Offset); err != nil {
					errs <- fmt.Errorf("%s commit: %w", n, err)
					return
				}
			}
			gotMu.Lock()
			got[n] = mine
			gotMu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, n := range consumers {
		recs := got[n]
		if len(recs) != total {
			t.Fatalf("%s read %d records, want %d", n, len(recs), total)
		}
		for i, r := range recs {
			if r.Offset != uint64(i) {
				t.Fatalf("%s: record %d has offset %d", n, i, r.Offset)
			}
			if sent[r.Offset] != string(r.Payload) {
				t.Fatalf("%s: offset %d = %q, producer sent %q", n, r.Offset, r.Payload, sent[r.Offset])
			}
		}
	}
	if s := tp.Stats(); s.Segments > 2 {
		t.Errorf("everything committed, yet %d segments remain", s.Segments)
	}
}
