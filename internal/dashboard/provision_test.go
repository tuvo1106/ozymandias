package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeStore records what provisioning asked for, and can be told to fail.
type fakeStore struct {
	rows    map[string]Row
	changed map[string]bool
	failOn  string
	calls   []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]Row{}, changed: map[string]bool{}}
}

func (f *fakeStore) UpsertProvisionedDashboard(_ context.Context, row Row, _ time.Time) (bool, error) {
	f.calls = append(f.calls, row.UID)
	if row.UID == f.failOn {
		return false, errors.New("the disk is on fire")
	}
	prev, existed := f.rows[row.UID]
	f.rows[row.UID] = row
	changed := !existed || string(prev.Definition) != string(row.Definition)
	f.changed[row.UID] = changed
	return changed, nil
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func def(uid, title string) string {
	return `{"uid":"` + uid + `","title":"` + title + `",` +
		`"widgets":[{"id":"w1","type":"note","layout":{"x":0,"y":0,"w":12,"h":1},"markdown":"hi"}]}`
}

func TestProvision_LoadsEveryFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "home.json", def("home", "Home"))
	write(t, dir, "checkout.json", def("checkout", "Checkout"))
	// Not JSON, not provisioned — a README next to the dashboards is normal.
	write(t, dir, "README.md", "these are the dashboards")

	store := newFakeStore()
	res := Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
	if res.Loaded != 2 || res.Changed != 2 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	if store.rows["home"].Title != "Home" {
		t.Errorf("home: %+v", store.rows["home"])
	}
	// The definition stored is the file's bytes, not a re-encoding: a diff of
	// the row against the file should be empty.
	if string(store.rows["home"].Definition) != def("home", "Home") {
		t.Errorf("the definition was rewritten:\n%s", store.rows["home"].Definition)
	}
}

// One team's typo must not stop everybody else's monitoring from starting.
func TestProvision_OneBadFileDoesNotStopTheRest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a-good.json", def("good", "Good"))
	write(t, dir, "b-broken.json", `{"uid":"broken","title":`)
	write(t, dir, "c-invalid.json", `{"uid":"invalid","title":"No widgets","widgets":[]}`)
	write(t, dir, "d-nouid.json", def("", "No uid"))
	write(t, dir, "e-alsogood.json", def("also", "Also good"))

	store := newFakeStore()
	res := Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
	if res.Loaded != 2 {
		t.Errorf("loaded %d, want the 2 good ones", res.Loaded)
	}
	if res.Failed != 3 {
		t.Errorf("failed %d, want 3", res.Failed)
	}
	for _, uid := range []string{"good", "also"} {
		if _, ok := store.rows[uid]; !ok {
			t.Errorf("%s was not provisioned", uid)
		}
	}
}

// A uid is what the upsert is keyed on. Defaulting it to the filename would
// mean renaming the file silently creates a second dashboard.
func TestProvision_AMissingUIDIsRefusedRatherThanGuessed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "orphan.json", def("", "Orphan"))
	store := newFakeStore()
	res := Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
	if res.Failed != 1 || res.Loaded != 0 {
		t.Fatalf("%+v", res)
	}
	if len(store.calls) != 0 {
		t.Errorf("it tried to store %v", store.calls)
	}
}

// `provisioning.paths` naming a directory an app has not mounted yet is a
// configuration that will become correct. It must not stop startup.
func TestProvision_AMissingDirectoryIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "home.json", def("home", "Home"))

	store := newFakeStore()
	res := Provision(context.Background(), store,
		[]string{filepath.Join(dir, "not-mounted-yet"), dir}, time.Unix(1, 0), nil)
	if res.Failed != 0 {
		t.Errorf("failed %d, want 0 — an absent directory is not a failure", res.Failed)
	}
	if res.Loaded != 1 {
		t.Errorf("loaded %d, want 1", res.Loaded)
	}
}

// A restart that changed nothing is the normal case, and has to be visible as
// such.
func TestProvision_ARestartWithNoEditsChangesNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "home.json", def("home", "Home"))
	store := newFakeStore()

	first := Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
	if first.Changed != 1 {
		t.Fatalf("%+v", first)
	}
	second := Provision(context.Background(), store, []string{dir}, time.Unix(2, 0), nil)
	if second.Loaded != 1 {
		t.Errorf("loaded %d on the restart", second.Loaded)
	}
	if second.Changed != 0 {
		t.Errorf("changed %d, want 0 — nothing was edited", second.Changed)
	}

	// Editing the file is a change.
	write(t, dir, "home.json", def("home", "Home, renamed"))
	third := Provision(context.Background(), store, []string{dir}, time.Unix(3, 0), nil)
	if third.Changed != 1 {
		t.Errorf("an edited file gave changed = %d", third.Changed)
	}
}

// Two files claiming one uid must resolve the same way on every startup, or a
// dashboard changes depending on the order the filesystem felt like.
func TestProvision_OrderIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "z-second.json", def("dup", "Z wins if last"))
	write(t, dir, "a-first.json", def("dup", "A wins if last"))

	for range 3 {
		store := newFakeStore()
		Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
		if got := store.rows["dup"].Title; got != "Z wins if last" {
			t.Fatalf("title %q — sorted order should leave z-second.json last", got)
		}
		if len(store.calls) != 2 || store.calls[0] != "dup" {
			t.Fatalf("calls %v", store.calls)
		}
	}
}

// Directories are applied in the order configured, so an app's own directory
// can deliberately override one of ozymandias's.
func TestProvision_LaterDirectoriesWin(t *testing.T) {
	base, app := t.TempDir(), t.TempDir()
	write(t, base, "home.json", def("home", "Stock home"))
	write(t, app, "home.json", def("home", "The app's home"))

	store := newFakeStore()
	Provision(context.Background(), store, []string{base, app}, time.Unix(1, 0), nil)
	if got := store.rows["home"].Title; got != "The app's home" {
		t.Errorf("title %q, want the later directory to win", got)
	}
}

// A store that fails is counted, not fatal, and does not stop the next file.
func TestProvision_AStoreFailureIsCountedAndSurvived(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.json", def("a", "A"))
	write(t, dir, "b.json", def("b", "B"))
	store := newFakeStore()
	store.failOn = "a"

	res := Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
	if res.Failed != 1 || res.Loaded != 1 {
		t.Fatalf("%+v", res)
	}
	if _, ok := store.rows["b"]; !ok {
		t.Error("the file after the failure was skipped")
	}
}

// Subdirectories are not scanned, so a fixture or a work-in-progress can live
// next to the real dashboards.
func TestProvision_DoesNotRecurse(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "wip")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "home.json", def("home", "Home"))
	write(t, sub, "draft.json", def("draft", "Draft"))

	store := newFakeStore()
	res := Provision(context.Background(), store, []string{dir}, time.Unix(1, 0), nil)
	if res.Loaded != 1 {
		t.Errorf("loaded %d, want only the top-level file", res.Loaded)
	}
	if _, ok := store.rows["draft"]; ok {
		t.Error("a file in a subdirectory was provisioned")
	}
}

func TestProvision_NoPathsIsFine(t *testing.T) {
	store := newFakeStore()
	res := Provision(context.Background(), store, nil, time.Unix(1, 0), nil)
	if res != (ProvisionResult{}) {
		t.Errorf("%+v", res)
	}
}
