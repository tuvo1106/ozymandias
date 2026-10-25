package api

import (
	"context"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/meta"
)

// TestMonitors_DisabledHasNoStatus: a disabled monitor's stored ALERT is the
// last time it ran, not now — the list and the nav badge leave it out.
func TestMonitors_DisabledHasNoStatus(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	send(t, h, "POST", "/api/v1/monitors", monitorBody)
	if _, err := db.Commit(context.Background(), meta.Commit{MonitorID: 1, Upsert: []meta.StateRow{
		{GroupKey: "queue:a", Group: map[string]string{"queue": "a"}, Status: "ALERT", Since: now, LastValue: math.NaN()}}}); err != nil {
		t.Fatal(err)
	}
	disabled := strings.Replace(monitorBody, `"tags"`, `"enabled": false, "tags"`, 1)
	if rec, _ := send(t, h, "PUT", "/api/v1/monitors/1", disabled); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if _, body := get(t, h, "/api/v1/monitors/1"); body["status"] != nil || body["enabled"] != false {
		t.Fatalf("get %v", body)
	}
	_, body := get(t, h, "/api/v1/monitors/status")
	if n := len(body["monitors_by_status"].(map[string]any)); n != 0 {
		t.Fatalf("status counts a disabled monitor: %v", body)
	}
	// Its states are kept for when it comes back.
	if st, _ := db.States(context.Background(), 1); len(st) != 1 {
		t.Fatalf("states %v", st)
	}
}

// TestMonitors_SkippedMonitorSaysSo: a stored definition that no longer
// compiles shows the reason and no status, and is left out of the badge;
// the answer is current right after a fix, holds for a disabled monitor, and
// holds on a server that is not running monitors.
func TestMonitors_SkippedMonitorSaysSo(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	broken := meta.MonitorRow{Name: "old", Type: "metric", Query: "avg(last_5m):avg:x{*} > 1",
		Message: "{{if .IsAlert}} @hook{{end}}", Enabled: true}
	if _, err := db.CreateMonitor(context.Background(), broken, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Commit(context.Background(), meta.Commit{MonitorID: 1, Upsert: []meta.StateRow{
		{GroupKey: "*", Status: "OK", Since: now, LastValue: math.NaN()}}}); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, h, "/api/v1/monitors/1")
	if p, _ := body["problem"].(string); !strings.Contains(p, "inside {{if}}") || body["status"] != nil {
		t.Fatalf("skipped monitor: %v", body)
	}
	_, list := get(t, h, "/api/v1/monitors?status=OK")
	if n := len(list["monitors"].([]any)); n != 0 {
		t.Fatalf("a skipped monitor listed as OK: %v", list)
	}
	if _, st := get(t, h, "/api/v1/monitors/status"); len(st["monitors_by_status"].(map[string]any)) != 0 {
		t.Fatalf("the badge counts a skipped monitor: %v", st)
	}
	// Fixed through the API: the response itself is current.
	fixed := `{"name":"old","type":"metric","query":"avg(last_5m):avg:x{*} > 1","message":"ok @hook"}`
	if rec, body := send(t, h, "PUT", "/api/v1/monitors/1", fixed); rec.Code != 200 || body["problem"] != nil {
		t.Fatalf("after the fix: %d %v", rec.Code, body)
	}
	if _, st := get(t, h, "/api/v1/monitors/status"); st["monitors_by_status"].(map[string]any)["OK"] != 1.0 {
		t.Fatalf("badge after the fix: %v", st)
	}
	// Disabled, and with no engine at all: still said.
	broken.Enabled = false
	if _, err := db.CreateMonitor(context.Background(), broken, now); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, h, "/api/v1/monitors/2"); body["problem"] == nil {
		t.Fatalf("disabled skipped monitor: %v", body)
	}
	mux := http.NewServeMux()
	(&Monitors{Store: db}).Register(mux)
	if _, body := get(t, mux, "/api/v1/monitors/2"); body["problem"] == nil {
		t.Fatalf("no engine: %v", body)
	}
}

// TestMonitors_ProblemCacheCoversEveryCompiledField: a problem caused by the
// name (not the query or message) clears when only the name is fixed, and a
// deleted monitor's entry is dropped.
func TestMonitors_ProblemCacheCoversEveryCompiledField(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	m := &Monitors{Store: db}
	mux := http.NewServeMux()
	m.Register(mux)
	row, err := db.CreateMonitor(context.Background(), meta.MonitorRow{Name: strings.Repeat("n", 300), Type: "metric",
		Query: "avg(last_5m):avg:x{*} > 1", Enabled: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, mux, "/api/v1/monitors/1"); body["problem"] == nil {
		t.Fatalf("a bad name is not a problem: %v", body)
	}
	row.Name = "short"
	if _, err := db.UpdateMonitor(context.Background(), row.ID, row, now); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, mux, "/api/v1/monitors/1"); body["problem"] != nil {
		t.Fatalf("fixed name still a problem: %v", body)
	}
	send(t, mux, "DELETE", "/api/v1/monitors/1", "")
	if len(m.problems) != 0 {
		t.Fatalf("deleted monitor still cached: %v", m.problems)
	}
	// Deleted behind the API's back (provisioning, another process): the
	// next list drops it.
	other, _ := db.CreateMonitor(context.Background(), meta.MonitorRow{Name: "x", Type: "metric",
		Query: "avg(last_5m):avg:x{*} > 1", Enabled: true}, now)
	get(t, mux, "/api/v1/monitors")
	if err := db.DeleteMonitor(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	get(t, mux, "/api/v1/monitors")
	if len(m.problems) != 0 {
		t.Fatalf("a monitor deleted elsewhere is still cached: %v", m.problems)
	}
}

// TestMonitors_ListWithStates: ?states=1 gives each running monitor's groups,
// worst first with their tags and values, and none for a disabled one, whose
// stored states are stale. Without it the list stays as it was.
func TestMonitors_ListWithStates(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	send(t, h, "POST", "/api/v1/monitors", monitorBody)
	if _, err := db.Commit(context.Background(), meta.Commit{MonitorID: 1, Upsert: []meta.StateRow{
		{GroupKey: "env:prod", Group: map[string]string{"env": "prod"}, Status: "OK", Since: now, LastValue: 1},
		{GroupKey: "env:staging", Group: map[string]string{"env": "staging"}, Status: "ALERT", Since: now, LastValue: 31},
	}}); err != nil {
		t.Fatal(err)
	}
	_, plain := get(t, h, "/api/v1/monitors")
	if _, ok := plain["monitors"].([]any)[0].(map[string]any)["group_states"]; ok {
		t.Errorf("group_states without ?states=1: %v", plain)
	}
	_, list := get(t, h, "/api/v1/monitors?states=1")
	gs, _ := list["monitors"].([]any)[0].(map[string]any)["group_states"].([]any)
	if len(gs) != 2 {
		t.Fatalf("group_states: %v", list)
	}
	first := gs[0].(map[string]any)
	if first["status"] != "ALERT" || first["value"] != 31.0 || first["tags"].(map[string]any)["env"] != "staging" || first["group"] != "env:staging" {
		t.Errorf("not worst first with tags and value: %v", gs)
	}

	disabled := strings.Replace(monitorBody, `"tags"`, `"enabled": false, "tags"`, 1)
	if rec, _ := send(t, h, "PUT", "/api/v1/monitors/1", disabled); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	_, list = get(t, h, "/api/v1/monitors?states=1")
	if _, ok := list["monitors"].([]any)[0].(map[string]any)["group_states"]; ok {
		t.Errorf("a disabled monitor's stale states were listed: %v", list)
	}
}
