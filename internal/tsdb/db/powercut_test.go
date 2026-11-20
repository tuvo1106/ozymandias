package db

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// openTracked opens a DB whose WAL fsync goes through a tracker, so the test can ask what a power
// cut would have left.
func openTracked(t *testing.T, opts Options) (*DB, *testutil.SyncTracker, string) {
	t.Helper()
	st := testutil.NewSyncTracker()
	opts.walSyncFile = st.SyncFile
	db, _, dir := open(t, opts)
	return db, st, dir
}

// appendN appends n batches, each one sample of its own series m<from+i>, one Append each.
func appendN(t *testing.T, db *DB, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		mustAppend(t, db, ss(fmt.Sprint("m", i), nil, sm(epoch.UnixMilli(), float64(i))))
	}
}

// survivors reopens the directory a power cut left and says which of the first n series came back.
func survivors(t *testing.T, cutDir string, n int) []int {
	t.Helper()
	db, _, _ := open(t, Options{Dir: cutDir, SyncOnAppend: true})
	var got []int
	for i := 0; i < n; i++ {
		set, err := db.Select(ctx, tsdb.Selector{Metric: fmt.Sprint("m", i)}, math.MinInt64, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		if dump(t, set) != "" {
			got = append(got, i)
		}
	}
	return got
}

func upTo(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func sameInts(a, b []int) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// What a power cut leaves of the metric WAL, for each way of syncing it. The queued consumer
// relies on the third row: Append writes without an fsync, Sync makes everything appended so far
// durable, and only then is the queue offset committed (ADR-0063). The second row is the control:
// without Sync the same appends are gone, which shows this harness really discards what was
// never fsynced, so the rows around it mean something.
func TestPowerCut_WhatSurvivesInTheMetricWAL(t *testing.T) {
	for _, tc := range []struct {
		name         string
		syncOnAppend bool
		beforeSync   int  // appends before the Sync
		doSync       bool // whether there is a Sync
		afterSync    int  // appends after it
		want         []int
	}{
		{"SyncOnAppend: Append alone is durable", true, 5, false, 0, upTo(5)},
		{"deferred, no Sync: nothing survives", false, 5, false, 0, nil},
		{"deferred, then Sync: everything survives", false, 5, true, 0, upTo(5)},
		{"appended after the Sync is lost, before it is not", false, 3, true, 2, upTo(3)},
		{"a second Sync moves the line", false, 2, true, 3, upTo(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, st, dir := openTracked(t, Options{SyncOnAppend: tc.syncOnAppend})
			appendN(t, db, 0, tc.beforeSync)
			if tc.doSync {
				if err := db.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			appendN(t, db, tc.beforeSync, tc.afterSync)
			cut := st.PowerCut(t, dir, filepath.Join(dir, walDirName), 0)
			if got := survivors(t, cut, tc.beforeSync+tc.afterSync); !sameInts(got, tc.want) {
				t.Fatalf("after a power cut, series %v came back, want %v", got, tc.want)
			}
		})
	}
}

// A disk may keep part of what was never synced, ending in the middle of a record. Whatever it
// keeps, what was synced is all there, what comes back is a prefix of what was appended (no gap,
// nothing invented), and the DB opens: the torn tail is repaired, not fatal.
func TestPowerCut_APartlyKeptTailNeverCostsWhatWasSynced(t *testing.T) {
	for _, extra := range []int64{1, 2, 7, 19, 40, 64, 150, 1 << 20} {
		t.Run(fmt.Sprint("keeps ", extra, " more bytes"), func(t *testing.T) {
			db, st, dir := openTracked(t, Options{SyncOnAppend: false})
			appendN(t, db, 0, 3)
			if err := db.Sync(); err != nil {
				t.Fatal(err)
			}
			appendN(t, db, 3, 4)
			cut := st.PowerCut(t, dir, filepath.Join(dir, walDirName), extra)
			got := survivors(t, cut, 7)
			if len(got) < 3 {
				t.Fatalf("a synced series was lost: %v", got)
			}
			if !sameInts(got, upTo(len(got))) {
				t.Fatalf("what came back is not a prefix of what was appended: %v", got)
			}
		})
	}
}

// The tracker only knows about fsyncs that go through the hook; if the DB stopped using it, every
// test above would see "nothing synced" and the Sync row would fail for a reason that has
// nothing to do with ordering. This says so first.
func TestPowerCut_TheDBSyncsThroughTheHook(t *testing.T) {
	db, st, dir := openTracked(t, Options{SyncOnAppend: false})
	appendN(t, db, 0, 1)
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	if st.Syncs() == 0 {
		t.Fatal("Sync did not reach the tracked fsync")
	}
	if _, err := os.Stat(filepath.Join(dir, walDirName)); err != nil {
		t.Fatal(err)
	}
}

// Sync flushes the segment being written; a segment the log rolled past was fsynced by the roll.
// This crosses several segment boundaries, which a fsync of only the last segment would not survive.
func TestPowerCut_SyncCoversSegmentsTheLogRolledPast(t *testing.T) {
	db, st, dir := openTracked(t, Options{SyncOnAppend: false, WALSegmentSize: 200})
	appendN(t, db, 0, 12)
	segs, err := filepath.Glob(filepath.Join(dir, walDirName, "*"))
	if err != nil || len(segs) < 3 {
		t.Fatalf("the appends made %d segments (%v); the test needs several", len(segs), err)
	}
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	appendN(t, db, 12, 2)
	cut := st.PowerCut(t, dir, filepath.Join(dir, walDirName), 0)
	// More than the 12 synced may come back: rolling into a new segment fsynced the old one. What
	// came back must be a prefix of what was appended.
	got := survivors(t, cut, 14)
	if len(got) < 12 || !sameInts(got, upTo(len(got))) {
		t.Fatalf("after a power cut, series %v came back, want a prefix with the 12 appended before the Sync", got)
	}
}
