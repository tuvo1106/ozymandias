package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor"
	"github.com/tuvo1106/ozymandias/internal/monitor/notify"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// fakeEngine stands in for the scheduler.
type fakeEngine struct {
	reloads int
	health  monitor.Health
	dryErr  error
	tested  []int64
}

func (f *fakeEngine) Reload()                { f.reloads++ }
func (f *fakeEngine) Health() monitor.Health { return f.health }
func (f *fakeEngine) DryRun(_ context.Context, c *spec.Compiled, _ time.Time) ([]monitor.GroupValue, error) {
	if f.dryErr != nil {
		return nil, f.dryErr
	}
	v := 12.5
	return []monitor.GroupValue{{Key: "queue:a", Group: map[string]string{"queue": "a"}, Value: &v, Status: "WARN"}}, nil
}
func (f *fakeEngine) Backtest(_ context.Context, c *spec.Compiled, id int64, from, to time.Time) (monitor.Backtest, error) {
	if to.Sub(from) > monitor.MaxBacktestRange {
		return monitor.Backtest{}, errors.New("backtest: too long")
	}
	v := 25.0
	return monitor.Backtest{From: from.UTC(), To: to.UTC(), Evaluations: 3, Final: map[string]string{"*": "ALERT"},
		Events: []monitor.BacktestEvent{{Ts: from.Add(time.Minute).UTC(), Group: "*", From: "OK", To: "ALERT", Value: &v, Notify: true, Reason: "transition"}},
		Series: []monitor.BacktestSeries{{Group: "*", Tags: map[string]string{}, Points: []monitor.TickValue{
			{Ts: from.Add(30 * time.Second), Value: math.NaN()}, {Ts: from.Add(time.Minute), Value: v}}}}}, nil
}
func (f *fakeEngine) TestNotification(_ context.Context, row meta.MonitorRow) (map[string]error, error) {
	f.tested = append(f.tested, row.ID)
	if !strings.Contains(row.Message, "@") {
		return nil, monitor.ErrNoMentions
	}
	return map[string]error{"hook": nil, "ops": errors.New("HTTP 500: nope")}, nil
}

const monitorBody = `{
  "uid": "jobs-backlog",
  "name": "Job queue is backing up",
  "type": "metric",
  "query": "avg(last_5m):avg:jobs.queue.depth{service:shop-api} by {queue} > 20",
  "options": {"thresholds": {"warning": 10, "critical_recovery": 15}, "notify_no_data": true, "renotify_interval_m": 60},
  "message": "Queue depth is {{.Value}} on {{.Group}} @hook",
  "tags": ["team:shop"]
}`

func monitorsAPI(t *testing.T) (http.Handler, *meta.DB, *fakeEngine, *testutil.FakeClock) {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := testutil.NewFakeClock(now)
	eng := &fakeEngine{health: monitor.Health{Monitors: 1}}
	mux := http.NewServeMux()
	(&Monitors{Store: db, Engine: eng, Clock: clock,
		Notifiers: []notify.Info{{Name: "hook", Type: "webhook", Ready: true}, {Name: "ops", Type: "discord", Problem: "environment variable DISCORD_WEBHOOK_URL is not set"}},
	}).Register(mux)
	return mux, db, eng, clock
}

func TestMonitors_CRUD(t *testing.T) {
	h, db, eng, clock := monitorsAPI(t)
	rec, body := send(t, h, "POST", "/api/v1/monitors", monitorBody)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/api/v1/monitors/1" || body["status"] != nil {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if eng.reloads != 1 {
		t.Errorf("create did not reload the scheduler")
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors", monitorBody); rec.Code != http.StatusConflict {
		t.Errorf("duplicate uid: %d", rec.Code)
	}
	// A state appears; the list reports the worst.
	if _, err := db.Commit(context.Background(), meta.Commit{MonitorID: 1, Upsert: []meta.StateRow{
		{GroupKey: "queue:a", Group: map[string]string{"queue": "a"}, Status: "WARN", Since: now, LastValue: 12},
		{GroupKey: "queue:b", Group: map[string]string{"queue": "b"}, Status: "ALERT", Since: now, LastValue: 30},
		{GroupKey: "queue:c", Group: map[string]string{"queue": "c"}, Status: "OK", Since: now, LastValue: math.NaN()},
	}}); err != nil {
		t.Fatal(err)
	}
	_, body = send(t, h, "GET", "/api/v1/monitors/1", "")
	if body["status"] != "ALERT" || body["groups"].(map[string]any)["WARN"] != 1.0 {
		t.Errorf("get: %v", body)
	}
	for q, n := range map[string]int{"": 1, "?tag=team:shop": 1, "?tag=team:other": 0, "?status=ALERT": 1, "?status=NO_DATA": 0, "?q=QUEUE": 1, "?q=zzz": 0} {
		_, body = send(t, h, "GET", "/api/v1/monitors"+q, "")
		if got := len(body["monitors"].([]any)); got != n {
			t.Errorf("list%s: %d, want %d", q, got, n)
		}
	}
	_, body = send(t, h, "GET", "/api/v1/monitors/1/states", "")
	states := body["states"].([]any)
	if len(states) != 3 || states[0].(map[string]any)["status"] != "ALERT" || states[2].(map[string]any)["value"] != nil {
		t.Errorf("states not worst-first, or NaN not null: %v", states)
	}
	clock.Advance(time.Minute)
	upd := strings.Replace(monitorBody, `"Job queue is backing up"`, `"Renamed"`, 1)
	rec, body = send(t, h, "PUT", "/api/v1/monitors/1", upd)
	if rec.Code != http.StatusOK || body["name"] != "Renamed" || body["updated_at"] == body["created_at"] {
		t.Fatalf("update: %d %v", rec.Code, body)
	}
	if rec, _ := send(t, h, "DELETE", "/api/v1/monitors/1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec, _ := send(t, h, "GET", "/api/v1/monitors/1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
	if eng.reloads != 3 {
		t.Errorf("reloads %d", eng.reloads)
	}
}

func TestMonitors_RefusesBadDefinitions(t *testing.T) {
	h, _, _, _ := monitorsAPI(t)
	for name, body := range map[string]string{
		"bad query":        strings.Replace(monitorBody, "> 20", "", 1),
		"unknown mention":  strings.Replace(monitorBody, "@hook", "@pagerduty", 1),
		"misspelt option":  strings.Replace(monitorBody, "notify_no_data", "notify_nodata", 1),
		"bad template":     strings.Replace(monitorBody, "{{.Value}}", "{{.Valu}}", 1),
		"recovery wrong":   strings.Replace(monitorBody, `"critical_recovery": 15`, `"critical_recovery": 25`, 1),
		"trailing content": monitorBody + "{}",
	} {
		rec, b := send(t, h, "POST", "/api/v1/monitors", body)
		if rec.Code != http.StatusBadRequest || b["error"] == "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec, b := send(t, h, "POST", "/api/v1/monitors", strings.Replace(monitorBody, "@hook", "@pagerduty", 1))
	if !strings.Contains(b["error"].(string), "@pagerduty") || !strings.Contains(b["error"].(string), "@hook") {
		t.Errorf("mention error does not name both: %s", rec.Body)
	}
	for _, p := range []string{"/api/v1/monitors/x", "/api/v1/monitors/0", "/api/v1/monitors/-1"} {
		if rec, _ := send(t, h, "GET", p, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec, _ := send(t, h, "GET", "/api/v1/monitors/42/states", ""); rec.Code != http.StatusNotFound {
		t.Errorf("states of missing: %d", rec.Code)
	}
}

func TestMonitors_ProvisionedIsReadOnlyButMutable(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	c, _ := spec.Compile(spec.Monitor{Name: "p", Type: spec.TypeMetric, Query: "avg(last_5m):avg:x{*} > 1"})
	if _, _, err := db.UpsertProvisionedMonitor(context.Background(), meta.MonitorRow{UID: "p", Name: c.Name, Type: "metric",
		Query: c.Query, Options: []byte(`{}`), Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	upd := strings.Replace(monitorBody, `"jobs-backlog"`, `"p"`, 1)
	if rec, b := send(t, h, "PUT", "/api/v1/monitors/1", upd); rec.Code != http.StatusConflict || !strings.Contains(b["error"].(string), "edit the file") {
		t.Errorf("update provisioned: %d %v", rec.Code, b)
	}
	if rec, _ := send(t, h, "DELETE", "/api/v1/monitors/1", ""); rec.Code != http.StatusConflict {
		t.Errorf("delete provisioned: %d", rec.Code)
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors/1/mute", ""); rec.Code != http.StatusOK {
		t.Errorf("mute provisioned: %d", rec.Code)
	}
}

func TestMonitors_Mute(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	send(t, h, "POST", "/api/v1/monitors", monitorBody)
	rec, b := send(t, h, "POST", "/api/v1/monitors/1/mute", `{"until": 1790003600}`)
	if rec.Code != http.StatusOK || b["muted_until"] != 1790003600.0 {
		t.Fatalf("mute: %d %v", rec.Code, b)
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors/1/mute", `{"until": 1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("mute into the past: %d", rec.Code)
	}
	_, b = send(t, h, "POST", "/api/v1/monitors/1/mute", "")
	if b["muted_until"] == nil {
		t.Errorf("indefinite mute: %v", b)
	}
	_, b = send(t, h, "POST", "/api/v1/monitors/1/unmute", "")
	if b["muted_until"] != nil {
		t.Errorf("unmute: %v", b)
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors/9/mute", ""); rec.Code != http.StatusNotFound {
		t.Errorf("mute missing: %d", rec.Code)
	}
	evs, _ := db.Events(context.Background(), meta.EventQuery{MonitorID: 1})
	if len(evs) != 3 || evs[0].Kind != "unmuted" || !strings.Contains(evs[2].Text, "until 2026") {
		t.Errorf("mute events %+v", evs)
	}
}

func TestMonitors_Events(t *testing.T) {
	h, db, _, _ := monitorsAPI(t)
	ctx := context.Background()
	for i, k := range []string{"transition", "notify_ok", "transition"} {
		_ = db.AddEvent(ctx, meta.EventRow{Ts: now.Add(time.Duration(i) * time.Minute), Kind: k, MonitorID: int64(1 + i%2), Value: math.NaN()})
	}
	for q, n := range map[string]int{
		"": 3, "?monitor_id=1": 2, "?kind=notify_ok": 1, "?kind=transition,notify_ok": 3,
		"?from=1790000060": 2, "?to=1790000060": 2, "?limit=1": 1,
	} {
		rec, b := send(t, h, "GET", "/api/v1/events"+q, "")
		if rec.Code != 200 || len(b["events"].([]any)) != n {
			t.Errorf("events%s: %d %s", q, rec.Code, rec.Body)
		}
	}
	for _, q := range []string{"?limit=0", "?limit=9999", "?monitor_id=x"} {
		if rec, _ := send(t, h, "GET", "/api/v1/events"+q, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("events%s: %d", q, rec.Code)
		}
	}
}

func TestMonitors_ValidateBacktestTestNotification(t *testing.T) {
	h, _, eng, _ := monitorsAPI(t)
	_, b := send(t, h, "POST", "/api/v1/monitors/validate", monitorBody)
	if b["valid"] != true || b["grouped"] != true || len(b["groups"].([]any)) != 1 {
		t.Errorf("validate ok: %v", b)
	}
	rec, b := send(t, h, "POST", "/api/v1/monitors/validate", strings.Replace(monitorBody, "> 20", "> x", 1))
	if rec.Code != 200 || b["valid"] != false || len(b["errors"].([]any)) == 0 {
		t.Errorf("validate bad: %d %v", rec.Code, b)
	}
	eng.dryErr = errors.New("no metric store")
	if _, b = send(t, h, "POST", "/api/v1/monitors/validate", monitorBody); b["eval_error"] != "no metric store" || b["valid"] != true {
		t.Errorf("validate eval error: %v", b)
	}
	rec, b = send(t, h, "POST", "/api/v1/monitors/backtest", `{"monitor": `+monitorBody+`, "from": 1789990000, "to": 1790000000}`)
	if rec.Code != 200 || b["evaluations"] != 3.0 {
		t.Errorf("backtest: %d %v", rec.Code, b)
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors/backtest", `{"monitor": `+monitorBody+`, "from": 1, "to": 1790000000}`); rec.Code != 400 {
		t.Errorf("backtest range: %d", rec.Code)
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors/backtest", `{"monitor": {"name": "x"}}`); rec.Code != 400 {
		t.Errorf("backtest invalid: %d", rec.Code)
	}
	send(t, h, "POST", "/api/v1/monitors", monitorBody)
	rec, b = send(t, h, "POST", "/api/v1/monitors/1/test-notification", "")
	res := b["results"].([]any)
	if rec.Code != 200 || len(res) != 2 || res[0].(map[string]any)["ok"] != true || res[1].(map[string]any)["error"] != "HTTP 500: nope" {
		t.Errorf("test notification: %d %v", rec.Code, b)
	}
	if rec, _ := send(t, h, "POST", "/api/v1/monitors/7/test-notification", ""); rec.Code != 404 {
		t.Errorf("test missing: %d", rec.Code)
	}
}

func TestMonitors_NoEngine(t *testing.T) {
	db, _ := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	defer func() { _ = db.Close() }()
	mux := http.NewServeMux()
	(&Monitors{Store: db}).Register(mux)
	send(t, mux, "POST", "/api/v1/monitors", strings.Replace(monitorBody, " @hook", "", 1))
	if rec, _ := send(t, mux, "POST", "/api/v1/monitors/backtest", `{"monitor": `+strings.Replace(monitorBody, " @hook", "", 1)+`}`); rec.Code != 503 {
		t.Errorf("backtest without engine: %d", rec.Code)
	}
	if rec, _ := send(t, mux, "POST", "/api/v1/monitors/1/test-notification", ""); rec.Code != 503 {
		t.Errorf("test without engine: %d", rec.Code)
	}
	_, b := send(t, mux, "GET", "/api/v1/monitors/status", "")
	if b["running"] != false {
		t.Errorf("status %v", b)
	}
	_, b = send(t, mux, "GET", "/api/v1/notifiers", "")
	if len(b["notifiers"].([]any)) != 0 {
		t.Errorf("notifiers %v", b)
	}
}

// TestMonitors_NoSecretsInResponses: every endpoint's body is checked for the
// one secret the test plants, which is the claim notify.Info makes.
func TestMonitors_NoSecretsInResponses(t *testing.T) {
	const secret = "https://discord.example/api/webhooks/1/SUPERSECRET"
	_, infos := notify.Build(map[string]config.Notifier{"ops": {Type: "discord", WebhookURLEnv: "D"}},
		func(string) string { return secret }, nil)
	db, _ := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	defer func() { _ = db.Close() }()
	mux := http.NewServeMux()
	(&Monitors{Store: db, Engine: &fakeEngine{}, Notifiers: infos}).Register(mux)
	send(t, mux, "POST", "/api/v1/monitors", strings.Replace(monitorBody, "@hook", "@ops", 1))
	for _, p := range []string{"/api/v1/notifiers", "/api/v1/monitors", "/api/v1/monitors/1", "/api/v1/monitors/status", "/api/v1/events"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if strings.Contains(rec.Body.String(), "SUPERSECRET") {
			t.Errorf("%s leaks the secret: %s", p, rec.Body)
		}
	}
}

// TestMonitors_Golden pins whole bodies (L7), in order against one handler.
func TestMonitors_Golden(t *testing.T) {
	h, db, _, clock := monitorsAPI(t)
	steps := []struct {
		name, method, path, body string
		before                   func()
	}{
		{"monitors-create", "POST", "/api/v1/monitors", monitorBody, nil},
		{"monitors-get", "GET", "/api/v1/monitors/1", "", func() {
			_, _ = db.Commit(context.Background(), meta.Commit{MonitorID: 1,
				Upsert: []meta.StateRow{{GroupKey: "queue:a", Group: map[string]string{"queue": "a"}, Status: "ALERT",
					Since: now, Breaches: 1, LastValue: 25, LastEval: now, LastData: now.Add(-40 * time.Second), LastNotified: now}},
				Events: []meta.EventRow{{Ts: now, Kind: "transition", MonitorID: 1, GroupKey: "queue:a", From: "OK", To: "ALERT", Value: 25}}})
		}},
		{"monitors-list", "GET", "/api/v1/monitors", "", nil},
		{"monitors-states", "GET", "/api/v1/monitors/1/states", "", nil},
		{"monitors-events", "GET", "/api/v1/events?monitor_id=1", "", nil},
		{"monitors-mute", "POST", "/api/v1/monitors/1/mute", `{"until": 1790003600}`, func() { clock.Advance(time.Minute) }},
		{"monitors-validate", "POST", "/api/v1/monitors/validate", monitorBody, nil},
		{"monitors-validate-error", "POST", "/api/v1/monitors/validate", strings.Replace(monitorBody, `"warning": 10`, `"warning": 30`, 1), nil},
		{"monitors-backtest", "POST", "/api/v1/monitors/backtest", `{"monitor": ` + monitorBody + `, "from": 1789913600, "to": 1790000000}`, nil},
		{"monitors-status", "GET", "/api/v1/monitors/status", "", nil},
		{"monitors-notifiers", "GET", "/api/v1/notifiers", "", nil},
		{"monitors-missing", "GET", "/api/v1/monitors/999", "", nil},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.before != nil {
				s.before()
			}
			rec, _ := send(t, h, s.method, s.path, s.body)
			checkGolden(t, s.name, rec)
		})
	}
}

func checkGolden(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	var pretty bytes.Buffer
	pretty.WriteString(http.StatusText(rec.Code) + "\n")
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q is not JSON", rec.Body.String())
	}
	enc := json.NewEncoder(&pretty)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden", name+".json")
	if *updateGolden {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — regenerate with: go test ./internal/api -run Golden -update-golden", err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Errorf("the response no longer matches %s.\n--- committed ---\n%s\n--- now ---\n%s", path, want, pretty.String())
	}
}
