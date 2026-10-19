package api

import (
	"context"
	"math"
	"net/http"
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
