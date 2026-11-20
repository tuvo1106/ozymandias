package sketchstore

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

const syncTestMs = 1_700_000_000_000

// openOnStrictFS opens a store on a filesystem that forgets whatever was not fsynced when the
// power is cut. The directory is made durable first: a cut would otherwise take the store with
// it and every case would read "lost", which is the fixture's doing, not the store's.
func openOnStrictFS(t *testing.T, deferSync bool) (*Store, *vfs.MemFS, Options) {
	t.Helper()
	fs := vfs.NewStrictMem()
	if err := fs.MkdirAll("sketches", 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := fs.OpenDir("")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	o := Options{Dir: "sketches", Clock: testutil.NewFakeClock(time.UnixMilli(syncTestMs)), DeferSync: deferSync, fs: fs}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, fs, o
}

// powerCut is what the store holds after the power fails.
func powerCut(t *testing.T, fs *vfs.MemFS, s *Store, o Options) *Store {
	t.Helper()
	fs.SetIgnoreSyncs(true) // from here on nothing reaches the "disk"
	_ = s.Close()
	fs.ResetToSyncedState()
	fs.SetIgnoreSyncs(false)
	s2, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	return s2
}

func sketchHeld(t *testing.T, s *Store, metric string) bool {
	t.Helper()
	got, err := s.Read(context.Background(), ref(metric), syncTestMs, syncTestMs)
	if err != nil {
		t.Fatal(err)
	}
	return len(got) > 0
}

// Under a power cut right after the call that made the promise: the default Append is durable
// by itself (direct mode acknowledges on it); a deferred one is not, and Sync is what makes it so.
func TestSync_WhatSurvivesAPowerCut(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deferSync  bool
		syncBefore bool
		survives   bool
	}{
		{"default: Append is durable by itself", false, false, true},
		{"deferred: Append alone is not durable", true, false, false},
		{"deferred: Append then Sync is durable", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fs, o := openOnStrictFS(t, tc.deferSync)
			mustAppend(t, s, Entry{Series: ref("lat"), Points: []Point{{TimeMs: syncTestMs, Sketch: sketchOf(t, 1, 2, 3)}}})
			if tc.syncBefore {
				if err := s.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if got := sketchHeld(t, powerCut(t, fs, s, o), "lat"); got != tc.survives {
				t.Fatalf("after a power cut the sketch is present = %v, want %v", got, tc.survives)
			}
		})
	}
}

// One Sync covers every Append before it, however many: the point of deferring.
func TestSync_OneSyncCoversManyAppends(t *testing.T) {
	s, fs, o := openOnStrictFS(t, true)
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, n := range names {
		mustAppend(t, s, Entry{Series: ref(n), Points: []Point{{TimeMs: syncTestMs, Sketch: sketchOf(t, 1, 2)}}})
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	s2 := powerCut(t, fs, s, o)
	for _, n := range names {
		if !sketchHeld(t, s2, n) {
			t.Fatalf("series %q, appended before the Sync, was lost to the power cut", n)
		}
	}
}

func TestSync_AClosedStoreReportsItInsteadOfPretending(t *testing.T) {
	s, _, _ := openOnStrictFS(t, true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err == nil {
		t.Fatal("Sync on a closed store returned nil: a caller would commit what is not durable")
	}
}
