package logstore

import (
	"context"
	"testing"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func dedupe(o *Options) { o.DedupeHead = true }

func TestAppend_ARedeliveredBatchIsStoredOnce(t *testing.T) {
	s, _ := openStore(t, t.TempDir(), dedupe)
	defer func() { _ = s.Close() }()
	base := t0.UnixMilli()
	batch := []wire.Log{mkLog(base, "api", "info", "a", 1), mkLog(base+1, "api", "info", "b", 2)}
	for range 3 {
		if err := s.Append(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if got := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{}); len(got) != 2 {
		t.Fatalf("%d logs after three deliveries of a two-log batch, want 2", len(got))
	}
}

// Identical lines inside one batch are two events, not a redelivery.
func TestAppend_RepeatsWithinOneBatchAreKept(t *testing.T) {
	s, _ := openStore(t, t.TempDir(), dedupe)
	defer func() { _ = s.Close() }()
	l := mkLog(t0.UnixMilli(), "api", "warn", "retrying", 1)
	if err := s.Append(context.Background(), []wire.Log{l, l, l}); err != nil {
		t.Fatal(err)
	}
	if got := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{}); len(got) != 3 {
		t.Fatalf("%d logs, want the 3 that were sent", len(got))
	}
}

// Append must leave its argument as it found it: the consumer publishes the
// same slice to live tail afterwards.
func TestAppend_DoesNotRewriteTheCallersBatch(t *testing.T) {
	s, _ := openStore(t, t.TempDir(), dedupe)
	defer func() { _ = s.Close() }()
	a, b := mkLog(t0.UnixMilli(), "api", "info", "a", 1), mkLog(t0.UnixMilli()+1, "api", "info", "b", 2)
	if err := s.Append(context.Background(), []wire.Log{a}); err != nil {
		t.Fatal(err)
	}
	batch := []wire.Log{a, b}
	if err := s.Append(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if batch[0].Message != "a" || batch[1].Message != "b" || len(batch) != 2 {
		t.Fatalf("caller's batch was rewritten: %q %q", batch[0].Message, batch[1].Message)
	}
}

// After a crash the head is rebuilt from the WAL; a redelivery of the batch
// that was in it must still be recognized.
func TestAppend_RedeliveryAfterRestartIsStoredOnce(t *testing.T) {
	dir := t.TempDir()
	orig, _ := openStore(t, dir, dedupe)
	defer func() { _ = orig.Close() }()
	s := orig
	batch := []wire.Log{mkLog(t0.UnixMilli(), "api", "info", "a", 1)}
	if err := s.Append(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	// Not Close: that seals the head into a block, which is the window this
	// store does not cover. A crash leaves the entries in the WAL only.
	s, _ = openStore(t, crashCopy(t, dir), dedupe)
	defer func() { _ = s.Close() }()
	if err := s.Append(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{}); len(got) != 1 {
		t.Fatalf("%d logs, want 1", len(got))
	}
}

// The known limit, pinned so it is a decision and not a surprise: once a
// batch's entries have been sealed into a block the head no longer remembers
// them, and a redelivery is stored again. Closing the window needs either a
// per-stream memory that survives a seal and a restart, or an idempotency
// token from the queue; neither is built (docs/notes/M7.md). If this test
// starts failing, the window has closed — update the notes.
func TestAppend_RedeliveryAfterSealIsNotRecognized(t *testing.T) {
	s, _ := openStore(t, t.TempDir(), dedupe)
	defer func() { _ = s.Close() }()
	batch := []wire.Log{mkLog(t0.UnixMilli(), "api", "info", "a", 1)}
	for range 2 {
		if err := s.Append(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if got := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{}); len(got) != 2 {
		t.Fatalf("%d logs, want 2 (the documented window)", len(got))
	}
}

// Without the option a genuine repeat in a later request is kept: the dedupe
// is for queue consumers, not for every sender.
func TestAppend_WithoutDedupeHeadRepeatsAreKept(t *testing.T) {
	s, _ := openStore(t, t.TempDir())
	defer func() { _ = s.Close() }()
	batch := []wire.Log{mkLog(t0.UnixMilli(), "api", "info", "a", 1)}
	for range 2 {
		if err := s.Append(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if got := all(t, s, "service:api", wideFrom, wideTo, SearchOpts{}); len(got) != 2 {
		t.Fatalf("%d logs, want 2", len(got))
	}
}
