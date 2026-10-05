package tailer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistry_RoundTripsAndWritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "registry.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Set("file:1:2", Entry{Path: "/a", Offset: 42, LastSeen: 100})
	r.Set("docker:abc", Entry{Path: "c", TS: 9, LastSeen: 200})
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(filepath.Dir(path)); len(es) != 1 {
		t.Fatalf("temp files left behind: %v", es)
	}
	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := r2.Get("file:1:2"); !ok || e.Offset != 42 {
		t.Fatalf("%+v %v", e, ok)
	}
	if r2.Len() != 2 {
		t.Fatal(r2.Len())
	}
	// Unchanged means no rewrite.
	r2.Set("file:1:2", Entry{Path: "/a", Offset: 42, LastSeen: 100})
	if r2.dirt {
		t.Fatal("setting an identical entry marked the registry dirty")
	}
	if n := r2.Prune(time.Unix(100, 0)); n != 0 {
		t.Fatalf("an entry seen exactly at the cutoff was pruned")
	}
	r2.Delete("docker:abc")
	r2.Delete("nothing")
	if n := r2.Prune(time.Unix(150, 0)); n != 1 || r2.Len() != 0 {
		t.Fatalf("pruned %d, %d left", n, r2.Len())
	}
	if err := r2.Flush(); err != nil {
		t.Fatal(err)
	}
	r3, _ := OpenRegistry(path)
	if r3.Len() != 0 {
		t.Fatal("deletes were not persisted")
	}
}

func TestRegistry_CorruptOrMissingFilesStartEmpty(t *testing.T) {
	dir := t.TempDir()
	if r, err := OpenRegistry(filepath.Join(dir, "none.json")); err != nil || r.Len() != 0 {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	r, err := OpenRegistry(bad)
	if err == nil || r == nil || r.Len() != 0 {
		t.Fatalf("a corrupt registry must be reported and replaced by an empty usable one: %v", err)
	}
	mem, _ := OpenRegistry("")
	mem.Set("k", Entry{})
	if err := mem.Flush(); err != nil {
		t.Fatal("an in-memory registry has nothing to flush")
	}
}

// Polling an idle file stamps "seen now" every second; that alone must not
// make the registry dirty, or the agent rewrites and fsyncs it once a second for ever.
func TestRegistry_AnIdlePollDoesNotDirtyTheRegistry(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Set("file:1", Entry{Path: "/a", Offset: 10, LastSeen: 1000})
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	r.Set("file:1", Entry{Path: "/a", Offset: 10, LastSeen: 1001})
	if regDirty(r) {
		t.Fatal("a one-second-newer LastSeen made the registry dirty")
	}
	r.Set("file:1", Entry{Path: "/a", Offset: 11, LastSeen: 1002})
	if !regDirty(r) {
		t.Fatal("a new offset did not")
	}
	_ = r.Flush()
	r.Set("file:1", Entry{Path: "/a", Offset: 11, LastSeen: 1002 + lastSeenRefresh})
	if !regDirty(r) {
		t.Fatal("a stamp that has gone stale was never refreshed")
	}
}

func regDirty(r *Registry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dirt
}

func TestFiles_AScanForgetsEntriesNothingHasSeenForAMonth(t *testing.T) {
	r := newRig(t, FileSource{})
	r.reg.Set("docker:gone", Entry{Path: "old", TS: 1, LastSeen: r.clk.Now().Add(-RegistryTTL - time.Hour).Unix()})
	r.reg.Set("docker:recent", Entry{Path: "new", TS: 1, LastSeen: r.clk.Now().Unix()})
	r.write("a.log", "")
	r.poll(DefaultScanInterval)
	if _, ok := r.reg.Get("docker:gone"); ok {
		t.Error("an entry unseen for over a month survived a scan")
	}
	if _, ok := r.reg.Get("docker:recent"); !ok {
		t.Error("a recent entry was pruned")
	}
}
