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

// TestMonitors_SkippedMonitorSaysSo: a monitor the scheduler skips (its
// stored definition no longer compiles) shows the reason and no status, since
// its stored states are as stale as a disabled one's.
func TestMonitors_SkippedMonitorSaysSo(t *testing.T) {
	h, db, eng, _ := monitorsAPI(t)
	send(t, h, "POST", "/api/v1/monitors", monitorBody)
	if _, err := db.Commit(context.Background(), meta.Commit{MonitorID: 1, Upsert: []meta.StateRow{
		{GroupKey: "queue:a", Group: map[string]string{"queue": "a"}, Status: "OK", Since: now, LastValue: math.NaN()}}}); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, h, "/api/v1/monitors/1"); body["problem"] != nil || body["status"] != "OK" {
		t.Fatalf("running monitor: %v", body)
	}
	eng.problems = map[int64]string{1: "message: @x is inside {{if}}"}
	_, body := get(t, h, "/api/v1/monitors/1")
	if body["problem"] != "message: @x is inside {{if}}" || body["status"] != nil {
		t.Fatalf("skipped monitor: %v", body)
	}
	_, list := get(t, h, "/api/v1/monitors?status=OK")
	if n := len(list["monitors"].([]any)); n != 0 {
		t.Fatalf("a skipped monitor listed as OK: %v", list)
	}
	if _, st := get(t, h, "/api/v1/monitors/status"); len(st["monitors_by_status"].(map[string]any)) != 0 {
		t.Fatalf("the badge counts a skipped monitor: %v", st)
	}
}
