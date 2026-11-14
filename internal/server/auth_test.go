package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
