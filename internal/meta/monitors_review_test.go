package meta

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// TestMonitors_QueryChangeResetsStates: the reset happens in the transaction
// that changes the query, so a scheduler that never saw the old query (a
// restart, a monitor edited while disabled) does not inherit its groups.
func TestMonitors_QueryChangeResetsStates(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	m, _ := d.CreateMonitor(ctx, mon("m"), t0)
	seed := func() {
		t.Helper()
		if _, err := d.Commit(ctx, Commit{MonitorID: m.ID, Upsert: []StateRow{
			{GroupKey: "host:a", Group: map[string]string{"host": "a"}, Status: "ALERT", Since: t0, LastValue: math.NaN()}}}); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	// Same query, other fields: states stay.
	m.Name = "renamed"
	if _, err := d.UpdateMonitor(ctx, m.ID, m, t0); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.States(ctx, m.ID); len(st) != 1 {
		t.Fatalf("a rename reset states: %v", st)
	}
	m.Query += " "
	if _, err := d.UpdateMonitor(ctx, m.ID, m, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.States(ctx, m.ID); len(st) != 0 {
		t.Fatalf("query change kept states: %v", st)
	}
	evs, _ := d.Events(ctx, EventQuery{MonitorID: m.ID, Kinds: []string{"reset"}})
	if len(evs) != 1 {
		t.Fatalf("reset events %+v", evs)
	}

	// Provisioning does the same.
	p := mon("p")
	p.UID = "p"
	pr, _, err := d.UpsertProvisionedMonitor(ctx, p, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Commit(ctx, Commit{MonitorID: pr.ID, Upsert: []StateRow{
		{GroupKey: "*", Status: "ALERT", Since: t0, LastValue: math.NaN()}}}); err != nil {
		t.Fatal(err)
	}
	p.Query += " "
	if _, _, err := d.UpsertProvisionedMonitor(ctx, p, t0); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.States(ctx, pr.ID); len(st) != 0 {
		t.Fatalf("provisioned query change kept states: %v", st)
	}
}

// TestMonitors_CommitRefusesDeletedOrDisabled: an evaluation that was in
// flight when its monitor went away writes nothing, and says why.
func TestMonitors_CommitRefusesDeletedOrDisabled(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	c := func(id int64) Commit {
		return Commit{MonitorID: id,
			Upsert: []StateRow{{GroupKey: "*", Status: "ALERT", Since: t0, LastValue: math.NaN()}},
			Outbox: []OutboxRow{{Created: t0, MonitorID: id, GroupKey: "*", Notifier: "hook", Payload: []byte(`{}`)}}}
	}
	gone, _ := d.CreateMonitor(ctx, mon("gone"), t0)
	if err := d.DeleteMonitor(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Commit(ctx, c(gone.ID)); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("deleted: %v", err)
	}
	off, _ := d.CreateMonitor(ctx, mon("off"), t0)
	off.Enabled = false
	if _, err := d.UpdateMonitor(ctx, off.ID, off, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Commit(ctx, c(off.ID)); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("disabled: %v", err)
	}
	// Changed under it: the evaluation of the old query is refused, one of
	// the current query is not.
	ch, _ := d.CreateMonitor(ctx, mon("changed"), t0)
	stale := c(ch.ID)
	stale.Query = "avg(last_5m):avg:old{*} > 1"
	if _, err := d.Commit(ctx, stale); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("stale query: %v", err)
	}
	if st, _ := d.States(ctx, 0); len(st) != 0 {
		t.Fatalf("states written: %v", st)
	}
	current := c(ch.ID)
	current.Query = ch.Query
	if _, err := d.Commit(ctx, current); err != nil {
		t.Fatalf("current query: %v", err)
	}
	if err := d.DeleteMonitor(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if ob, _ := d.Outbox(ctx); len(ob) != 0 {
		t.Fatalf("outbox written: %v", ob)
	}
}

// TestMonitors_ReleaseOrphans: a provisioned monitor whose uid is not kept
// becomes API-managed, with an event; kept and API-made ones are untouched.
func TestMonitors_ReleaseOrphans(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	for _, uid := range []string{"kept", "orphan"} {
		r := mon(uid)
		r.UID = uid
		if _, _, err := d.UpsertProvisionedMonitor(ctx, r, t0); err != nil {
			t.Fatal(err)
		}
	}
	api, _ := d.CreateMonitor(ctx, mon("api"), t0)
	got, err := d.ReleaseOrphanedMonitors(ctx, []string{"kept"}, t0)
	if err != nil || len(got) != 1 || got[0] != "orphan" {
		t.Fatalf("released %v %v", got, err)
	}
	rows, _ := d.Monitors(ctx)
	var orphan MonitorRow
	for _, r := range rows {
		if want := r.UID == "kept"; r.Provisioned != want {
			t.Errorf("%s provisioned=%v", r.Name, r.Provisioned)
		}
		if r.UID == "orphan" {
			orphan = r
		}
	}
	if _, err := d.UpdateMonitor(ctx, orphan.ID, orphan, t0); err != nil {
		t.Errorf("released monitor still refuses edits: %v", err)
	}
	evs, _ := d.Events(ctx, EventQuery{Kinds: []string{"released"}})
	if len(evs) != 1 || evs[0].MonitorID == api.ID {
		t.Fatalf("events %+v", evs)
	}
	// Idempotent.
	if got, _ := d.ReleaseOrphanedMonitors(ctx, []string{"kept"}, t0); len(got) != 0 {
		t.Fatalf("released twice: %v", got)
	}
}
