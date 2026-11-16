package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func authServer(t *testing.T, mod func(*config.Ozyd)) (*Server, http.Handler) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Auth.Enabled = true
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(cfg, Options{Logger: quiet, Getenv: func(k string) string {
		if k == "OZYMANDIAS_ADMIN_PASSWORD" {
			return "s3cret"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, s.Handler()
}

func do(h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newKey(t *testing.T, h http.Handler, limits string) string {
	t.Helper()
	w := do(h, "POST", "/api/v1/keys", fmt.Sprintf(`{"name":"t","limits":%s}`, limits), "Authorization", "Bearer s3cret")
	if w.Code != 201 {
		t.Fatalf("creating a key: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	i := strings.Index(body, `"key":"`) + len(`"key":"`)
	return body[i : i+strings.Index(body[i:], `"`)]
}

const seriesBody = `{"series":[{"metric":"auth.count","type":"count","interval":10,"tags":["env:t"],"points":[[1790000000,3]]}]}`

func TestAuth_EnabledWithoutAPasswordRefusesToStart(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Enabled = true
	if _, err := New(cfg, Options{Logger: quiet, Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("ozyd started with auth on and no admin password: it would be open")
	}
}

func TestAuth_OffMeansAbsentNotBroken(t *testing.T) {
	s, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := s.Handler()
	if w := do(h, "POST", "/v1/series", seriesBody); w.Code != http.StatusAccepted {
		t.Fatalf("intake without a key, auth off: %d", w.Code)
	}
	for _, p := range [][2]string{{"POST", "/api/v1/keys"}, {"GET", "/api/v1/keys"}, {"POST", "/api/v1/auth/login"}} {
		if w := do(h, p[0], p[1], "{}"); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s with auth off answered %d: a key endpoint must not exist", p[0], p[1], w.Code)
		}
	}
}

func TestAuth_EveryIntakeEndpointNeedsAKey(t *testing.T) {
	_, h := authServer(t, nil)
	for _, p := range []string{"/v1/series", "/v1/sketches", "/v1/logs", "/v1/traces"} {
		if w := do(h, "POST", p, "{}"); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a key: %d, want 401", p, w.Code)
		}
	}
	key := newKey(t, h, `{}`)
	if w := do(h, "POST", "/v1/series", seriesBody, wire.HeaderKey, key); w.Code != http.StatusAccepted {
		t.Fatalf("with a key: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/v1/metrics", "/api/v1/dashboards", "/api/v1/monitors", "/debug/vars"} {
		if w := do(h, "GET", p, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a login: %d, want 401", p, w.Code)
		}
		if w := do(h, "GET", p, "", wire.HeaderKey, key); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with an ingest key: %d, want 401", p, w.Code)
		}
		if w := do(h, "GET", p, "", "Authorization", "Bearer s3cret"); w.Code != http.StatusOK {
			t.Errorf("GET %s as admin: %d", p, w.Code)
		}
	}
	if w := do(h, "GET", "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("/healthz needs no login: %d", w.Code)
	}
}

// Rejections and admitted requests are both filed under their route, not "unmatched".
func TestAuth_RequestsAreCountedUnderTheirRoute(t *testing.T) {
	s, h := authServer(t, nil)
	key := newKey(t, h, `{}`)
	do(h, "POST", "/v1/series", seriesBody, wire.HeaderKey, key)
	do(h, "POST", "/v1/series", seriesBody)
	do(h, "GET", "/api/v1/metrics", "")
	routes := map[string]bool{}
	for _, p := range s.reg.Snapshot() {
		if p.Name != "ozy.http.requests" {
			continue
		}
		for _, tg := range p.Tags {
			if strings.HasPrefix(tg, "route:") {
				routes[strings.TrimPrefix(tg, "route:")] = true
			}
		}
	}
	for _, want := range []string{"POST /v1/series", "auth-reject"} {
		if !routes[want] {
			t.Errorf("no ozy.http.requests series for route %q; got %v", want, routes)
		}
	}
	if routes["unmatched"] {
		t.Errorf("a request was filed under \"unmatched\": %v", routes)
	}
}

func TestAuth_LogByteAndSpanLimitsReachTheIntake(t *testing.T) {
	_, h := authServer(t, nil)
	key := newKey(t, h, `{"log_bytes_per_day": 200, "spans_per_sec": 1}`)
	logs := `{"logs":[{"ts":1790000000000,"message":"` + strings.Repeat("x", 150) + `","status":"info","service":"svc"}]}`
	if w := do(h, "POST", "/v1/logs", logs, wire.HeaderKey, key); w.Code != http.StatusAccepted {
		t.Fatalf("first log batch: %d %s", w.Code, w.Body)
	}
	w := do(h, "POST", "/v1/logs", logs, wire.HeaderKey, key)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second log batch: %d (Retry-After %q), want 429 with a retry time", w.Code, w.Header().Get("Retry-After"))
	}

	var sb strings.Builder
	sb.WriteString(`{"spans":[`)
	for i := range wire.MaxSpansPerRequest {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"trace_id":"%032x","span_id":"%016x","service":"s","name":"n","resource":"r","type":"web","start":1790000000000000,"duration":5,"error":0}`, i+1, i+1)
	}
	sb.WriteString(`]}`)
	if w := do(h, "POST", "/v1/traces", sb.String(), wire.HeaderKey, key); w.Code != http.StatusAccepted {
		t.Fatalf("a full batch of spans: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/v1/traces", sb.String(), wire.HeaderKey, key); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a second full batch inside the second: %d, want 429", w.Code)
	}
}

func TestAuth_ARevokedKeyIsRefusedOnTheRunningServer(t *testing.T) {
	_, h := authServer(t, nil)
	key := newKey(t, h, `{}`)
	if w := do(h, "DELETE", "/api/v1/keys/1", "", "Authorization", "Bearer s3cret"); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/series", seriesBody, wire.HeaderKey, key); w.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key was accepted: %d", w.Code)
	}
}

// Keys must survive a restart of the server, from the metadata database.
func TestAuth_KeysSurviveARestart(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Enabled = true
	getenv := func(string) string { return "s3cret" }
	s1, err := New(cfg, Options{Logger: quiet, Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	key := newKey(t, s1.Handler(), `{}`)
	_ = s1.Close()
	s2, err := New(cfg, Options{Logger: quiet, Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if w := do(s2.Handler(), "POST", "/v1/series", seriesBody, wire.HeaderKey, key); w.Code != http.StatusAccepted {
		t.Fatalf("after a restart: %d", w.Code)
	}
}

// The password-missing path is the one an operator hits on a first try, and it
// must give back what it opened: a second New on the same data directory (a
// supervisor restarting in process, a test) must not find it held.
func TestAuth_AFailedStartReleasesTheDataDirectory(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Enabled = true
	if _, err := New(cfg, Options{Logger: quiet, Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("started with no password")
	}
	s, err := New(cfg, Options{Logger: quiet, Getenv: func(string) string { return "pw" }})
	if err != nil {
		t.Fatalf("the data directory was still held after a failed start: %v", err)
	}
	_ = s.Close()
}

// A key's series limits reach the intake and the sender is told, in both pipeline modes:
// a refused series is an item-level rejection in the 202, not a counter somewhere else.
func TestAuth_SeriesLimitsAreAnsweredAtIntake(t *testing.T) {
	_, h := authServer(t, nil)
	key := newKey(t, h, `{"max_series_per_metric": 3, "max_active_series": 4}`)
	body := func(metric string, from, to int, ts int64) string {
		var sb strings.Builder
		sb.WriteString(`{"series":[`)
		for i := from; i < to; i++ {
			if i > from {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"metric":%q,"type":"count","interval":10,"tags":["user:u%d"],"points":[[%d,1]]}`, metric, i, ts)
		}
		sb.WriteString(`]}`)
		return sb.String()
	}
	reply := func(w *httptest.ResponseRecorder) (accepted, rej int, errs []string) {
		t.Helper()
		var r struct {
			Accepted, Rejected int
			Errors             []string
		}
		if w.Code != http.StatusAccepted {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r.Accepted, r.Rejected, r.Errors
	}

	// 5 users on one metric: 3 fit.
	a, r, errs := reply(do(h, "POST", "/v1/series", body("m.a", 0, 5, 1790000000), wire.HeaderKey, key))
	if a != 3 || r != 2 || len(errs) != 2 || !strings.Contains(errs[0], "active series for this metric") {
		t.Fatalf("per-metric: accepted %d rejected %d errs %v, want 3 and 2 naming the limit", a, r, errs)
	}
	// The same three again (a later point: a repeated one is a no-op in direct mode) are not new: all admitted.
	if a, r, _ = reply(do(h, "POST", "/v1/series", body("m.a", 0, 3, 1790000010), wire.HeaderKey, key)); a != 3 || r != 0 {
		t.Fatalf("resending the admitted series: accepted %d rejected %d", a, r)
	}
	// A second metric has room for one more series before the key's total of 4.
	a, r, errs = reply(do(h, "POST", "/v1/series", body("m.b", 0, 3, 1790000000), wire.HeaderKey, key))
	if a != 1 || r != 2 || !strings.Contains(strings.Join(errs, " "), "4 active series") {
		t.Fatalf("total: accepted %d rejected %d errs %v, want 1 and 2 naming the total", a, r, errs)
	}
	// Another key is unaffected.
	other := newKey(t, h, `{}`)
	if a, r, _ = reply(do(h, "POST", "/v1/series", body("m.c", 0, 5, 1790000000), wire.HeaderKey, other)); a != 5 || r != 0 {
		t.Fatalf("another key: accepted %d rejected %d", a, r)
	}
	// And the refusals are counted by reason.
	for _, reason := range []string{"series_per_metric", "series_total"} {
		if !strings.Contains(do(h, "GET", "/debug/vars", "", "Authorization", "Bearer s3cret").Body.String(), `"reason:`+reason+`"`) {
			t.Errorf("ozy.intake.rejected{reason:%s} is not exposed", reason)
		}
	}
}

// A saved tag rule reaches the intake: the dropped tag is not in the stored series, the drop is
// counted, the key's series limit sees the merged series, and the rules need the admin login.
func TestTagConfig_RulesApplyAtIntakeAndNeedTheAdminLogin(t *testing.T) {
	s, h := authServer(t, nil)
	feedStores(t, s)
	admin := []string{"Authorization", "Bearer s3cret"}
	rule := `{"metric":"tc.count","mode":"deny","keys":["user"]}`
	if w := do(h, "PUT", "/api/v1/metrics/tag-config", rule); w.Code != http.StatusUnauthorized {
		t.Fatalf("a rule written without the admin login: %d, want 401", w.Code)
	}
	if w := do(h, "PUT", "/api/v1/metrics/tag-config", `{"metric":"m","mode":"nope","keys":["a"]}`, admin...); w.Code != http.StatusBadRequest {
		t.Fatalf("an invalid rule: %d, want 400", w.Code)
	}
	if w := do(h, "PUT", "/api/v1/metrics/tag-config", rule, admin...); w.Code != http.StatusOK {
		t.Fatalf("saving a rule: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/api/v1/metrics/tag-config", "", admin...); !strings.Contains(w.Body.String(), `"tc.count"`) {
		t.Fatalf("the rule is not listed: %s", w.Body)
	}

	key := newKey(t, h, `{"max_series_per_metric": 1}`)
	// Three users, one real series: the rule merges them, so a limit of one series per
	// metric admits all three points (the first creates the series, the others join it).
	var sb strings.Builder
	sb.WriteString(`{"series":[`)
	for i := 0; i < 3; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"metric":"tc.count","type":"count","interval":10,"tags":["env:t","user:u%d"],"points":[[%d,1]]}`, i, 1790000000+10*i)
	}
	sb.WriteString(`]}`)
	w := do(h, "POST", "/v1/series", sb.String(), wire.HeaderKey, key)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"rejected":0`) {
		t.Fatalf("three users under a deny rule and a limit of one series: %d %s", w.Code, w.Body)
	}
	query := func(q string) string {
		return do(h, "GET", "/api/v1/query?q="+url.QueryEscape(q)+"&from=1789999990&to=1790000100", "", admin...).Body.String()
	}
	var byEnv string
	for i := 0; i < 100 && !strings.Contains(byEnv, `"env":"t"`); i++ { // queued mode applies after it answers
		byEnv = query("sum:tc.count{*} by {env}")
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(byEnv, `"env":"t"`) || strings.Count(byEnv, `"points"`) != 1 {
		t.Fatalf("the three points did not land in the one merged series: %s", byEnv)
	}
	if byUser := query("sum:tc.count{*} by {user}"); strings.Contains(byUser, `"user":`) {
		t.Fatalf("the stored series still carries the dropped tag: %s", byUser)
	}
	if !strings.Contains(do(h, "GET", "/debug/vars", "", admin...).Body.String(), `"ozy.intake.tags_dropped"`) {
		t.Error("ozy.intake.tags_dropped is not exposed")
	}

	if w := do(h, "DELETE", "/api/v1/metrics/tag-config?metric=tc.count", "", admin...); w.Code != http.StatusOK {
		t.Fatalf("deleting the rule: %d %s", w.Code, w.Body)
	}
	// With the rule gone the same three users are three new series (their tags differ from
	// the merged one the key's single slot holds), and the key's limit of one refuses all three.
	body2 := strings.ReplaceAll(sb.String(), "1790000", "1790001")
	w = do(h, "POST", "/v1/series", body2, wire.HeaderKey, key)
	if !strings.Contains(w.Body.String(), `"rejected":3`) {
		t.Fatalf("after the rule was removed: %s, want all three users refused by the series limit", w.Body)
	}
}

// A key at its series limit cannot mint metric names either: a refused series registers nothing.
func TestAuth_ARefusedSeriesRegistersNoMetricName(t *testing.T) {
	s, h := authServer(t, nil)
	key := newKey(t, h, `{"max_active_series": 2}`)
	send := func(metric string) {
		body := fmt.Sprintf(`{"series":[{"metric":%q,"type":"count","interval":10,"tags":[],"points":[[1790000000,1]]}]}`, metric)
		if w := do(h, "POST", "/v1/series", body, wire.HeaderKey, key); w.Code != http.StatusAccepted {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	send("name.first")
	send("name.second")
	for i := 0; i < 20; i++ {
		send(fmt.Sprintf("name.flood%d", i))
	}
	if _, ok := s.meta.Metric("name.first"); !ok {
		t.Fatal("the admitted series' metric was not registered")
	}
	for i := 0; i < 20; i++ {
		if _, ok := s.meta.Metric(fmt.Sprintf("name.flood%d", i)); ok {
			t.Fatalf("name.flood%d was refused by the key's limit and registered anyway", i)
		}
	}
	// A type conflict is refused after the limit check and must give the slot back.
	// Another key with two slots, one used: three different new series, each refused for its
	// type. If each kept its slot, the second would hit the limit instead of the conflict.
	key2 := newKey(t, h, `{"max_active_series": 2}`)
	if w := do(h, "POST", "/v1/series", `{"series":[{"metric":"name.other","type":"count","interval":10,"tags":[],"points":[[1790000000,1]]}]}`, wire.HeaderKey, key2); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for i := 0; i < 3; i++ {
		conflict := fmt.Sprintf(`{"series":[{"metric":"name.first","type":"gauge","interval":0,"tags":["x:%d"],"points":[[1790000001,1]]}]}`, i)
		if w := do(h, "POST", "/v1/series", conflict, wire.HeaderKey, key2); !strings.Contains(w.Body.String(), "conflict") {
			t.Fatalf("series %d: expected a type conflict, got %s", i, w.Body)
		}
	}
}

func TestTagConfig_PutIsStrictAboutItsBody(t *testing.T) {
	_, h := authServer(t, nil)
	admin := []string{"Authorization", "Bearer s3cret"}
	for name, body := range map[string]string{
		"a misspelt field": `{"metric":"m","mode":"deny","key":["user"]}`,
		"an extra field":   `{"metric":"m","mode":"deny","keys":["user"],"extra":1}`,
		"trailing data":    `{"metric":"m","mode":"deny","keys":["user"]} {"x":1}`,
		"upper-case key":   `{"metric":"m","mode":"deny","keys":["User"]}`,
	} {
		if w := do(h, "PUT", "/api/v1/metrics/tag-config", body, admin...); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, w.Code)
		}
	}
	if w := do(h, "DELETE", "/api/v1/metrics/tag-config?metric="+strings.Repeat("x", 300), "", admin...); w.Code != http.StatusBadRequest {
		t.Errorf("an over-long metric on DELETE: %d, want 400", w.Code)
	}
}
