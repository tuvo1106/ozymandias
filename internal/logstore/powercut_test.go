package logstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// pcStore opens a store whose WAL fsync goes through a tracker (see testutil.SyncTracker), and
// fails the test if a block is ever sealed: a sealed block is a file outside the WAL, which a
// power cut here would keep whole, so a test that sealed one would be too kind to the store.
func pcStore(t *testing.T, tune ...func(*Options)) (*Store, *testutil.SyncTracker, string) {
	t.Helper()
	st := testutil.NewSyncTracker()
	dir := t.TempDir()
	s, _ := openStore(t, dir, append([]func(*Options){func(o *Options) {
		o.NoSync = false
		o.BlockBytes = 8 << 20 // nothing here is meant to seal
		o.walSyncFile = st.SyncFile
	}}, tune...)...)
	var armed atomic.Bool
	armed.Store(true)
	s.afterBlock = func() {
		if armed.Load() {
			t.Error("a block was sealed: this test models only the WAL")
		}
	}
	t.Cleanup(func() { armed.Store(false); _ = s.Close() }) // Close seals the head, which is fine
	return s, st, dir
}

// pcAppend appends n batches of two logs each, one Append per batch: b<i>-000 and b<i>-001.
func pcAppend(t *testing.T, s *Store, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := s.Append(context.Background(), batchOf(fmt.Sprintf("b%d-", i), 0, 2, t0.UnixMilli())); err != nil {
			t.Fatal(err)
		}
	}
}

// pcMessages is every message pcAppend writes for batches 0..n-1, in order.
func pcMessages(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("b%d-000", i), fmt.Sprintf("b%d-001", i))
	}
	return out
}

// pcSurvivors reopens what a power cut left and returns the messages it holds.
func pcSurvivors(t *testing.T, cutDir string) []string {
	t.Helper()
	s := openSynced(t, cutDir)
	defer func() { _ = s.Close() }()
	return everything(t, s)
}

func pcWant(t *testing.T, got []string, want []string) {
	t.Helper()
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !equal(got, want) {
		t.Fatalf("after a power cut %d logs came back, want %d\n got  %v\n want %v", len(got), len(want), got, want)
	}
}

// pcPrefixOf checks that what came back is the first k messages of all, in order with no gap and
// nothing invented, and that k is at least synced.
func pcPrefixOf(t *testing.T, got []string, all []string, synced int) {
	t.Helper()
	k := len(got)
	if k < synced {
		t.Fatalf("a synced log was lost: %d came back, %d were synced", k, synced)
	}
	pcWant(t, got, all[:k])
}

// What a power cut leaves of the log store's WAL. The queued consumer relies on the third row
// (ADR-0063): DeferSync writes without an fsync, Sync makes everything appended so far durable,
// and only then is the queue offset committed. The second row is the control: without Sync the
// same appends are gone, which shows the harness really discards what was never fsynced.
func TestPowerCut_WhatSurvivesInTheLogWAL(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deferSync  bool
		beforeSync int
		doSync     bool
		afterSync  int
		wantBatch  int // batches that must come back, exactly
	}{
		{"default: Append alone is durable", false, 5, false, 0, 5},
		{"deferred, no Sync: nothing survives", true, 5, false, 0, 0},
		{"deferred, then Sync: everything survives", true, 5, true, 0, 5},
		{"appended after the Sync is lost, before it is not", true, 3, true, 2, 3},
		{"a second Sync moves the line", true, 2, true, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, dir := pcStore(t, func(o *Options) { o.DeferSync = tc.deferSync })
			pcAppend(t, s, 0, tc.beforeSync)
			if tc.doSync {
				if err := s.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			pcAppend(t, s, tc.beforeSync, tc.afterSync)
			cut := st.PowerCut(t, dir, filepath.Join(dir, "wal"), 0)
			pcWant(t, pcSurvivors(t, cut), pcMessages(tc.wantBatch))
		})
	}
}

// A disk may keep part of what was never synced, ending mid-record. What was synced is all there,
// what comes back is a prefix of what was appended, and the store opens.
func TestPowerCut_APartlyKeptTailNeverCostsWhatWasSynced(t *testing.T) {
	for _, extra := range []int64{1, 2, 7, 19, 40, 64, 150, 1 << 20} {
		t.Run(fmt.Sprint("keeps ", extra, " more bytes"), func(t *testing.T) {
			s, st, dir := pcStore(t, func(o *Options) { o.DeferSync = true })
			pcAppend(t, s, 0, 3)
			if err := s.Sync(); err != nil {
				t.Fatal(err)
			}
			pcAppend(t, s, 3, 4)
			pcPrefixOf(t, pcSurvivors(t, st.PowerCut(t, dir, filepath.Join(dir, "wal"), extra)), pcMessages(7), 6)
		})
	}
}

// Sync flushes the segment being written; the log fsyncs a segment it rolls past. This crosses
// several boundaries, which an fsync of only the last segment would not survive.
func TestPowerCut_SyncCoversSegmentsTheLogRolledPast(t *testing.T) {
	s, st, dir := pcStore(t, func(o *Options) { o.DeferSync = true; o.WALSegmentSize = 300 })
	pcAppend(t, s, 0, 12)
	if segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*")); len(segs) < 3 {
		t.Fatalf("the appends made %d segments; the test needs several", len(segs))
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	pcAppend(t, s, 12, 2)
	// More than the 24 synced may come back: rolling into a new segment fsynced the old one.
	pcPrefixOf(t, pcSurvivors(t, st.PowerCut(t, dir, filepath.Join(dir, "wal"), 0)), pcMessages(14), 24)
}

// With NoSync the store gives up the fsync entirely, Sync included: a power cut loses it all, which
// is what the option says and what queued mode documents for logs.no_sync.
func TestPowerCut_NoSyncLosesEverythingEvenAfterSync(t *testing.T) {
	s, st, dir := pcStore(t, func(o *Options) { o.NoSync = true; o.DeferSync = true })
	pcAppend(t, s, 0, 3)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	pcWant(t, pcSurvivors(t, st.PowerCut(t, dir, filepath.Join(dir, "wal"), 0)), nil)
}
