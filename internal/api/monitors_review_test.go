package api

import (
	"context"
	"math"
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

// TestMonitors_UncompilableRowSaysSo: a stored definition this build cannot
// compile is shown with the reason, since the scheduler skips it.
func TestMonitors_UncompilableRowSaysSo(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	if _, err := db.CreateMonitor(context.Background(), meta.MonitorRow{Name: "old", Type: "metric",
		Query: "avg(last_5m):avg:x{*} > 1", Message: "{{if .IsAlert}}@hook{{end}}", Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, h, "/api/v1/monitors/1")
	if p, _ := body["problem"].(string); !strings.Contains(p, "inside {{if}}") {
		t.Fatalf("problem %q", body["problem"])
	}
	send(t, h, "POST", "/api/v1/monitors", monitorBody)
	if _, body := get(t, h, "/api/v1/monitors/2"); body["problem"] != nil {
		t.Fatalf("a good monitor has a problem: %v", body["problem"])
	}
}
