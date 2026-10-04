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
