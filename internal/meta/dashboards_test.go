package meta

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dashDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

var t0 = time.Unix(1790000000, 0)

func row(title string) DashboardRow {
	return DashboardRow{Title: title, Definition: []byte(`{"title":"` + title + `"}`)}
}

func TestDashboards_CreateReadUpdateDelete(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)

	created, err := d.CreateDashboard(ctx, row("checkout"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 {
		t.Fatal("no id was assigned")
	}
	if !created.CreatedAt.Equal(t0) || !created.UpdatedAt.Equal(t0) {
		t.Errorf("timestamps %v / %v, want both %v", created.CreatedAt, created.UpdatedAt, t0)
	}

	got, err := d.Dashboard(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "checkout" || string(got.Definition) != `{"title":"checkout"}` {
		t.Fatalf("%+v", got)
	}

	// An update moves updated_at and leaves created_at alone: an edit is not a
	// new dashboard, and "when was this made" has to keep meaning that.
	t1 := t0.Add(time.Hour)
	next := row("checkout v2")
	updated, err := d.UpdateDashboard(ctx, created.ID, next, t1)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.CreatedAt.Equal(t0) {
		t.Errorf("created_at moved to %v", updated.CreatedAt)
	}
	if !updated.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v, want %v", updated.UpdatedAt, t1)
	}
	reread, err := d.Dashboard(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.Title != "checkout v2" {
		t.Errorf("title = %q", reread.Title)
	}

	if err := d.DeleteDashboard(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dashboard(ctx, created.ID); !errors.Is(err, ErrNoDashboard) {
		t.Errorf("after delete: %v, want ErrNoDashboard", err)
	}
}

func TestDashboards_MissingIsErrNoDashboard(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"read", func() error { _, err := d.Dashboard(ctx, 404); return err }},
		{"read by uid", func() error { _, err := d.DashboardByUID(ctx, "nope"); return err }},
		{"update", func() error { _, err := d.UpdateDashboard(ctx, 404, row("x"), t0); return err }},
		{"delete", func() error { return d.DeleteDashboard(ctx, 404) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrNoDashboard) {
				t.Errorf("%v, want ErrNoDashboard", err)
			}
		})
	}
}

// Provisioning runs at every startup. A restart that changed nothing must not
// look like an edit, or "when did this dashboard last change" stops being
// answerable.
func TestDashboards_ProvisioningAnUnchangedFileIsNotAWrite(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	r := row("home")
	r.UID = "home"

	first, changed, err := d.UpsertProvisionedDashboard(ctx, r, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("the first provisioning is a change")
	}
	if !first.Provisioned {
		t.Error("the row is not marked provisioned")
	}

	// The same file, an hour later.
	again, changed, err := d.UpsertProvisionedDashboard(ctx, r, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("an unchanged file was reported as a change")
	}
	if !again.UpdatedAt.Equal(t0) {
		t.Errorf("updated_at moved to %v for an unchanged file", again.UpdatedAt)
	}
	if again.ID != first.ID {
		t.Errorf("the id changed from %d to %d — upsert is by uid", first.ID, again.ID)
	}

	// An edited file is a change, and keeps the original created_at.
	edited := r
	edited.Definition = []byte(`{"title":"home","widgets":[]}`)
	third, changed, err := d.UpsertProvisionedDashboard(ctx, edited, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("an edited file is a change")
	}
	if !third.CreatedAt.Equal(t0) {
		t.Errorf("created_at moved to %v", third.CreatedAt)
	}
	if !third.UpdatedAt.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("updated_at = %v", third.UpdatedAt)
	}
	if third.ID != first.ID {
		t.Errorf("the id changed")
	}
}

// Writing to a provisioned dashboard would be undone at the next restart, with
// nothing anywhere explaining where the edit went.
func TestDashboards_AProvisionedDashboardIsReadOnlyToTheAPI(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	r := row("home")
	r.UID = "home"
	stored, _, err := d.UpsertProvisionedDashboard(ctx, r, t0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.UpdateDashboard(ctx, stored.ID, row("hijacked"), t0); !errors.Is(err, ErrProvisioned) {
		t.Errorf("update gave %v, want ErrProvisioned", err)
	}
	if err := d.DeleteDashboard(ctx, stored.ID); !errors.Is(err, ErrProvisioned) {
		t.Errorf("delete gave %v, want ErrProvisioned", err)
	}
	// And the refusal left it alone.
	after, err := d.Dashboard(ctx, stored.ID)
	if err != nil || after.Title != "home" {
		t.Errorf("%+v, %v", after, err)
	}
}

func TestDashboards_ProvisioningNeedsAUID(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	if _, _, err := d.UpsertProvisionedDashboard(ctx, row("no uid"), t0); err == nil {
		t.Error("a provisioned dashboard with no uid was accepted")
	}
}

// Many API-created dashboards have no uid, and SQLite's UNIQUE must not treat
// that as a collision — while two files claiming one uid still must collide.
func TestDashboards_ManyDashboardsMayHaveNoUID(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	for _, title := range []string{"a", "b", "c"} {
		if _, err := d.CreateDashboard(ctx, row(title), t0); err != nil {
			t.Fatalf("%s: %v", title, err)
		}
	}
	first := row("one")
	first.UID = "shared"
	if _, err := d.CreateDashboard(ctx, first, t0); err != nil {
		t.Fatal(err)
	}
	second := row("two")
	second.UID = "shared"
	if _, err := d.CreateDashboard(ctx, second, t0); err == nil {
		t.Error("two dashboards took the same uid")
	}
}

func TestDashboards_ListIsSortedByTitle(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	for _, title := range []string{"zeta", "alpha", "mu"} {
		if _, err := d.CreateDashboard(ctx, row(title), t0); err != nil {
			t.Fatal(err)
		}
	}
	list, err := d.Dashboards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range list {
		got = append(got, r.Title)
	}
	want := []string{"alpha", "mu", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	// The definitions come along, so a picker does not need a fetch per row.
	if len(list[0].Definition) == 0 {
		t.Error("the list dropped the definitions")
	}
}

// The table has to survive a reopen: it is created with IF NOT EXISTS on every
// Open, and a dashboard written by one process must be there for the next.
func TestDashboards_SurviveAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := first.CreateDashboard(ctx, row("persisted"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	got, err := second.Dashboard(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "persisted" {
		t.Errorf("%+v", got)
	}
}

// A duplicate uid is the caller's mistake, not a constraint violation to
// apologise for. It is reachable on the path the docs advertise — export from
// one ozyd, import into another — so it has to be classifiable.
func TestDashboards_ADuplicateUIDIsItsOwnError(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	first := row("one")
	first.UID = "shared"
	if _, err := d.CreateDashboard(ctx, first, t0); err != nil {
		t.Fatal(err)
	}

	second := row("two")
	second.UID = "shared"
	_, err := d.CreateDashboard(ctx, second, t0)
	if !errors.Is(err, ErrDuplicateUID) {
		t.Fatalf("create gave %v, want ErrDuplicateUID", err)
	}
	if !strings.Contains(err.Error(), "shared") {
		t.Errorf("%v does not name the uid", err)
	}

	// And on update, where the collision is with a different row.
	third := row("three")
	stored, err := d.CreateDashboard(ctx, third, t0)
	if err != nil {
		t.Fatal(err)
	}
	third.UID = "shared"
	if _, err := d.UpdateDashboard(ctx, stored.ID, third, t0); !errors.Is(err, ErrDuplicateUID) {
		t.Errorf("update gave %v, want ErrDuplicateUID", err)
	}
}

// Provisioning must not take over a uid that belongs to a dashboard somebody
// made through the API. Overwriting it would flip provisioned to 1 and leave
// the API answering 409 to every attempt to put it back — the same silent loss
// this package refuses everywhere else, pointed the other way.
func TestDashboards_ProvisioningWillNotStealAnAPIDashboardsUID(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	mine := row("mine")
	mine.UID = "home"
	stored, err := d.CreateDashboard(ctx, mine, t0)
	if err != nil {
		t.Fatal(err)
	}

	fromFile := row("theirs")
	fromFile.UID = "home"
	_, changed, err := d.UpsertProvisionedDashboard(ctx, fromFile, t0.Add(time.Hour))
	if !errors.Is(err, ErrDuplicateUID) {
		t.Fatalf("err = %v, want ErrDuplicateUID", err)
	}
	if changed {
		t.Error("it reported a change")
	}
	// Untouched: same title, same definition, still not provisioned.
	after, err := d.Dashboard(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "mine" || after.Provisioned || !after.UpdatedAt.Equal(t0) {
		t.Errorf("the API's dashboard was modified: %+v", after)
	}
	// And it is still writable through the API, which the takeover would have
	// ended.
	if _, err := d.UpdateDashboard(ctx, stored.ID, mine, t0); err != nil {
		t.Errorf("it is no longer writable: %v", err)
	}
}
