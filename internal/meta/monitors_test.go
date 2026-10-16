package meta

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func mon(name string) MonitorRow {
	return MonitorRow{Name: name, Type: "metric", Query: "avg(last_5m):avg:x{*} > 1",
		Options: json.RawMessage(`{"thresholds":{"critical":1}}`), Tags: []string{"team:a"}, Enabled: true}
}

func TestMonitors_CRUD(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	m, err := d.CreateMonitor(ctx, mon("b"), t0)
	if err != nil || m.ID == 0 {
		t.Fatal(m, err)
	}
	if _, err := d.CreateMonitor(ctx, mon("a"), t0); err != nil {
		t.Fatal(err)
	}
	list, err := d.Monitors(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Fatalf("list %+v %v", list, err)
	}
	got, err := d.Monitor(ctx, m.ID)
	if err != nil || got.Query != m.Query || string(got.Options) != string(m.Options) || got.Tags[0] != "team:a" ||
		!got.Enabled || !got.MutedUntil.IsZero() || !got.CreatedAt.Equal(t0) {
		t.Fatalf("got %+v %v", got, err)
	}
	up := mon("b2")
	up.Enabled = false
	if _, err := d.UpdateMonitor(ctx, m.ID, up, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = d.Monitor(ctx, m.ID)
	if got.Name != "b2" || got.Enabled || !got.UpdatedAt.Equal(t0.Add(time.Hour)) || !got.CreatedAt.Equal(t0) {
		t.Fatalf("updated %+v", got)
	}
	muted, err := d.SetMute(ctx, m.ID, t0.Add(2*time.Hour))
	if err != nil || !muted.MutedUntil.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("mute %+v %v", muted, err)
	}
	// An update keeps the mute: it is operational state, not definition.
	if after, _ := d.UpdateMonitor(ctx, m.ID, mon("b3"), t0); !after.MutedUntil.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("update dropped the mute: %v", after.MutedUntil)
	}
	if _, err := d.SetMute(ctx, 999, t0); !errors.Is(err, ErrNoMonitor) {
		t.Fatalf("mute missing: %v", err)
	}
	if err := d.DeleteMonitor(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Monitor(ctx, m.ID); !errors.Is(err, ErrNoMonitor) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestMonitors_UIDs(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	a := mon("a")
	a.UID = "x"
	if _, err := d.CreateMonitor(ctx, a, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateMonitor(ctx, a, t0); !errors.Is(err, ErrDuplicateUID) {
		t.Fatalf("duplicate: %v", err)
	}
	// Provisioning will not take an API-created monitor's uid.
	if _, _, err := d.UpsertProvisionedMonitor(ctx, a, t0); !errors.Is(err, ErrDuplicateUID) {
		t.Fatalf("provision over API row: %v", err)
	}
}

func TestMonitors_Provisioning(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	p := mon("p")
	p.UID = "prov"
	created, changed, err := d.UpsertProvisionedMonitor(ctx, p, t0)
	if err != nil || !changed || !created.Provisioned {
		t.Fatal(created, changed, err)
	}
	_, changed, err = d.UpsertProvisionedMonitor(ctx, p, t0.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("unchanged file rewrote the row: %v %v", changed, err)
	}
	if got, _ := d.Monitor(ctx, created.ID); !got.UpdatedAt.Equal(t0) {
		t.Fatalf("updated_at moved: %v", got.UpdatedAt)
	}
	p.Query = "avg(last_5m):avg:x{*} > 2"
	if _, changed, err = d.UpsertProvisionedMonitor(ctx, p, t0.Add(time.Hour)); err != nil || !changed {
		t.Fatalf("changed file: %v %v", changed, err)
	}
	if _, err := d.UpdateMonitor(ctx, created.ID, p, t0); !errors.Is(err, ErrProvisioned) {
		t.Fatalf("API update of provisioned: %v", err)
	}
	if err := d.DeleteMonitor(ctx, created.ID); !errors.Is(err, ErrProvisioned) {
		t.Fatalf("API delete of provisioned: %v", err)
	}
	// Muting a provisioned monitor is allowed, and survives re-provisioning.
	if _, err := d.SetMute(ctx, created.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p.Message = "changed"
	if got, _, _ := d.UpsertProvisionedMonitor(ctx, p, t0); !got.MutedUntil.Equal(t0.Add(time.Hour)) {
		t.Fatalf("provisioning cleared the mute")
	}
	if _, _, err := d.UpsertProvisionedMonitor(ctx, mon("nouid"), t0); err == nil {
		t.Fatal("provisioned without uid")
	}
}

func TestMonitors_CommitAndStates(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	m, _ := d.CreateMonitor(ctx, mon("m"), t0)
	st := StateRow{GroupKey: "queue:a", Group: map[string]string{"queue": "a"}, Status: "ALERT", Since: t0,
		Breaches: 2, LastValue: 25.5, LastEval: t0, LastData: t0.Add(-time.Minute), LastNotified: t0}
	fresh := StateRow{GroupKey: "*", Status: "OK", Since: t0, LastValue: math.NaN()}
	out, err := d.Commit(ctx, Commit{MonitorID: m.ID, Upsert: []StateRow{st, fresh},
		Events: []EventRow{{Ts: t0, Kind: "transition", MonitorID: m.ID, GroupKey: "queue:a", From: "OK", To: "ALERT", Value: 25.5}},
		Outbox: []OutboxRow{{Created: t0, MonitorID: m.ID, GroupKey: "queue:a", Notifier: "hook", Payload: json.RawMessage(`{"a":1}`)}}})
	if err != nil || len(out) != 1 || out[0].ID == 0 {
		t.Fatal(out, err)
	}
	states, err := d.States(ctx, m.ID)
	if err != nil || len(states) != 2 {
		t.Fatal(states, err)
	}
	if !math.IsNaN(states[0].LastValue) || !states[0].LastData.IsZero() || states[0].Group == nil {
		t.Errorf("fresh state round trip %+v", states[0])
	}
	g := states[1]
	if g.Status != "ALERT" || g.Breaches != 2 || g.LastValue != 25.5 || !g.LastNotified.Equal(t0) ||
		!g.LastData.Equal(t0.Add(-time.Minute)) || g.Group["queue"] != "a" {
		t.Errorf("state round trip %+v", g)
	}
	if all, _ := d.States(ctx, 0); len(all) != 2 {
		t.Errorf("all states %d", len(all))
	}
	ob, err := d.Outbox(ctx)
	if err != nil || len(ob) != 1 || string(ob[0].Payload) != `{"a":1}` {
		t.Fatal(ob, err)
	}
	if err := d.CompleteOutbox(ctx, ob[0].ID, EventRow{Ts: t0, Kind: "notify_ok", MonitorID: m.ID, Value: math.NaN()}); err != nil {
		t.Fatal(err)
	}
	if ob, _ := d.Outbox(ctx); len(ob) != 0 {
		t.Fatalf("outbox not emptied: %v", ob)
	}
	evs, err := d.Events(ctx, EventQuery{MonitorID: m.ID})
	if err != nil || len(evs) != 2 || evs[0].Kind != "notify_ok" || !math.IsNaN(evs[0].Value) || evs[1].Value != 25.5 {
		t.Fatalf("events %+v %v", evs, err)
	}
	if evs, _ := d.Events(ctx, EventQuery{Kinds: []string{"transition"}}); len(evs) != 1 {
		t.Fatalf("kind filter %v", evs)
	}
	if evs, _ := d.Events(ctx, EventQuery{From: t0.Add(time.Second)}); len(evs) != 0 {
		t.Fatalf("from filter %v", evs)
	}
	// Delete a group.
	if _, err := d.Commit(ctx, Commit{MonitorID: m.ID, Delete: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if states, _ := d.States(ctx, m.ID); len(states) != 1 {
		t.Fatalf("after delete %v", states)
	}
	if n, err := d.PruneEvents(ctx, t0.Add(time.Second)); err != nil || n != 2 {
		t.Fatalf("prune %d %v", n, err)
	}
}

// TestMonitors_CommitIsAtomic: a failure partway through a commit leaves no
// state, no event and no queued notification — the property that stops a
// notification being sent for a state that was never persisted.
func TestMonitors_CommitIsAtomic(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	m, _ := d.CreateMonitor(ctx, mon("m"), t0)
	if _, err := d.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON notify_outbox WHEN NEW.notifier = 'boom'
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := d.Commit(ctx, Commit{MonitorID: m.ID,
		Upsert: []StateRow{{GroupKey: "*", Status: "ALERT", Since: t0, LastValue: 1}},
		Events: []EventRow{{Ts: t0, Kind: "transition", MonitorID: m.ID}},
		Outbox: []OutboxRow{{Created: t0, MonitorID: m.ID, Notifier: "ok", Payload: json.RawMessage(`{}`)},
			{Created: t0, MonitorID: m.ID, Notifier: "boom", Payload: json.RawMessage(`{}`)}}})
	if err == nil {
		t.Fatal("commit succeeded")
	}
	states, _ := d.States(ctx, m.ID)
	evs, _ := d.Events(ctx, EventQuery{})
	ob, _ := d.Outbox(ctx)
	if len(states)+len(evs)+len(ob) != 0 {
		t.Fatalf("partial commit: %d states, %d events, %d outbox", len(states), len(evs), len(ob))
	}
}

func TestMonitors_DeleteCascades(t *testing.T) {
	ctx := context.Background()
	d := dashDB(t)
	m, _ := d.CreateMonitor(ctx, mon("m"), t0)
	other, _ := d.CreateMonitor(ctx, mon("o"), t0)
	for _, id := range []int64{m.ID, other.ID} {
		if _, err := d.Commit(ctx, Commit{MonitorID: id, Upsert: []StateRow{{GroupKey: "*", Status: "OK", Since: t0}},
			Events: []EventRow{{Ts: t0, Kind: "transition", MonitorID: id}},
			Outbox: []OutboxRow{{Created: t0, MonitorID: id, Notifier: "n", Payload: json.RawMessage(`{}`)}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.DeleteMonitor(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	states, _ := d.States(ctx, 0)
	evs, _ := d.Events(ctx, EventQuery{})
	ob, _ := d.Outbox(ctx)
	if len(states) != 1 || len(evs) != 1 || len(ob) != 1 || states[0].MonitorID != other.ID {
		t.Fatalf("cascade: %v %v %v", states, evs, ob)
	}
}
