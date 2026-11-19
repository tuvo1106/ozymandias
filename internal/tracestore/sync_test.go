package tracestore

import (
	"testing"

	"github.com/cockroachdb/pebble/vfs"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// powerCut returns what a store holds after the power fails: Pebble's filesystem keeps only
// what was fsynced, so a write that was never synced is gone. It is the test the ordering of
// the queue consumer's commit rests on: a store that has not synced has not made anything safe.
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

func openOnStrictFS(t *testing.T, mod func(*Options)) (*Store, *vfs.MemFS, Options) {
	t.Helper()
	fs := vfs.NewStrictMem()
	// The directory itself must be durable before the test starts, or a power cut takes the
	// whole store with it and every case reads as "lost": that is the test's fixture, not the
	// store's behaviour under test.
	if err := fs.MkdirAll("traces", 0o755); err != nil {
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
	o := Options{Dir: "traces", Clock: testutil.NewFakeClock(t0), fs: fs}
	mod(&o)
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, fs, o
}

func traceHeld(t *testing.T, s *Store, trace int) bool {
	t.Helper()
	got, err := s.Trace(ctx, tid(trace))
	if err != nil {
		t.Fatal(err)
	}
	return len(got) > 0
}

// The three promises, under a power cut right after the call that made them:
//   - default: Append returns only once the spans are durable (direct mode acknowledges on it);
//   - deferred: Append alone is not durable, and Sync is what makes it so;
//   - NoSync: nothing is, which is what it is for.
func TestSync_WhatSurvivesAPowerCut(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mod        func(*Options)
		syncBefore bool
		survives   bool
	}{
		{"default: Append is durable by itself", func(o *Options) {}, false, true},
		{"deferred: Append alone is not durable", func(o *Options) { o.DeferSync = true }, false, false},
		{"deferred: Append then Sync is durable", func(o *Options) { o.DeferSync = true }, true, true},
		{"NoSync: Sync does not make it durable", func(o *Options) { o.NoSync = true }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fs, o := openOnStrictFS(t, tc.mod)
			if err := s.Append(ctx, []wire.Span{sp(1, 1, "api", 0, 5_000_000)}); err != nil {
				t.Fatal(err)
			}
			if tc.syncBefore {
				if err := s.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if got := traceHeld(t, powerCut(t, fs, s, o), 1); got != tc.survives {
				t.Fatalf("after a power cut the span is present = %v, want %v", got, tc.survives)
			}
		})
	}
}

// One Sync covers every Append before it, however many: the point of deferring.
func TestSync_OneSyncCoversManyAppends(t *testing.T) {
	s, fs, o := openOnStrictFS(t, func(o *Options) { o.DeferSync = true })
	for i := 1; i <= 20; i++ {
		if err := s.Append(ctx, []wire.Span{sp(i, i, "api", 0, 5_000_000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	s2 := powerCut(t, fs, s, o)
	for i := 1; i <= 20; i++ {
		if !traceHeld(t, s2, i) {
			t.Fatalf("trace %d, appended before the Sync, was lost to the power cut", i)
		}
	}
}

func TestSync_AClosedStoreReportsItInsteadOfPretending(t *testing.T) {
	s, _, _ := openOnStrictFS(t, func(o *Options) { o.DeferSync = true })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err == nil {
		t.Fatal("Sync on a closed store returned nil: a caller would commit what is not durable")
	}
}
