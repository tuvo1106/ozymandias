package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent"
	agentconfig "github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/agent/logpipeline"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// logStack is a real ozyd and a real agent tailing the files in dir, on a fake
// clock, for the acceptance tests that need several kinds of log at once.
type logStack struct {
	t   *testing.T
	srv *Server
	clk *testutil.FakeClock
	dir string
}

func startLogStack(t *testing.T, sources func(dir string) []agentconfig.LogSource) *logStack {
	t.Helper()
	clk := testutil.NewFakeClock(time.Unix(1790000000, 0))
	srv, err := New(testConfig(t), Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); _ = srv.Close() })
	dir := t.TempDir()
	acfg := agentconfig.Default()
	acfg.Hostname = "box"
	acfg.Tags = []string{"env:e2e"}
	acfg.Intake.URL = ts.URL
	acfg.Statsd.Enabled = false
	acfg.Collectors.Docker.Enabled = false
	acfg.Collectors.Host.Enabled = false
	acfg.HTTP.ShutdownTimeout = time.Second
	acfg.Logs.Enabled = true
	acfg.Logs.Sources = sources(dir)
	a, err := agent.New(acfg, agent.Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "log poller not started")
	return &logStack{t: t, srv: srv, clk: clk, dir: dir}
}

func (s *logStack) write(name, content string) {
	s.t.Helper()
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		s.t.Fatal(err)
	}
}

// tick advances the agent's clock one poll interval (and the multiline flush).
func (s *logStack) tick() { s.clk.Advance(time.Second) }

type apiLog struct {
	Message string         `json:"message"`
	Status  string         `json:"status"`
	Service string         `json:"service"`
	Attrs   map[string]any `json:"attrs"`
}

func (s *logStack) search(q string) []apiLog {
	s.t.Helper()
	v := url.Values{"q": {q}, "limit": {"1000"}, "from": {"0"}, "to": {fmt.Sprint(s.clk.Now().Add(time.Hour).UnixMilli())}}
	rec := httptest.NewRecorder()
	s.srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/logs?"+v.Encode(), nil))
	if rec.Code != 200 {
		s.t.Fatalf("search %q: %d %s", q, rec.Code, rec.Body.String())
	}
	var out struct {
		Logs []apiLog `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		s.t.Fatal(err)
	}
	return out.Logs
}

// waitFor ticks the clock until the search returns want logs (the agent polls
// and posts asynchronously).
func (s *logStack) waitFor(q string, want int) []apiLog {
	s.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		s.tick()
		got := s.search(q)
		if len(got) == want {
			return got
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("search %q: %d logs, want %d: %+v", q, len(got), want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func e2eSources(dir string) []agentconfig.LogSource {
	file := func(glob, svc, source, multiline string) agentconfig.LogSource {
		return agentconfig.LogSource{
			Type: "file", Path: filepath.Join(dir, glob), Service: svc, StartPosition: "beginning",
			Multiline: agentconfig.Multiline{StartPattern: multiline},
			Spec:      logpipeline.Spec{Source: source, RateLimit: -1},
		}
	}
	return []agentconfig.LogSource{
		file("winston.log", "web-api", "winston", ""),
		file("rails.log", "orders-api", "rails", ""),
		file("worker.log", "worker", "python", `^(DEBUG|INFO|WARNING|ERROR|CRITICAL)`),
	}
}

// The milestone's search acceptance criteria, on synthetic logs shaped like the
// real ones (ADR-0038): slow requests by structured attribute, Rails requests
// grouped from interleaved lines, a Python traceback as one event.
func TestAcceptance_SearchOverWinstonRailsAndPythonLogs(t *testing.T) {
	s := startLogStack(t, e2eSources)
	ms := func() int64 { return s.clk.Now().UnixMilli() }
	s.write("winston.log", fmt.Sprintf(`{"level":"info","message":"api","timestamp":%d,"tag":"api","method":"GET","path":"/api/comics","status":200,"ms":12}
{"level":"info","message":"api","timestamp":%d,"tag":"api","method":"GET","path":"/api/comics/42","status":200,"ms":340}
{"level":"info","message":"db","timestamp":%d,"tag":"db","ms":900}
{"level":"warn","message":"api","timestamp":%d,"tag":"api","method":"POST","path":"/api/comics","status":404,"ms":250}
`, ms(), ms(), ms(), ms()))
	s.write("rails.log", `[8f3a1c7e-5b2d] Started GET "/orders?page=2" for 127.0.0.1 at 2026-09-21 14:13:20 +0000
[1d2e3f40-aaaa] Started POST "/orders" for 10.0.0.5 at 2026-09-21 14:13:20 +0000
[8f3a1c7e-5b2d] Processing by OrdersController#index as JSON
[1d2e3f40-aaaa] Processing by OrdersController#create as JSON
[1d2e3f40-aaaa]   Parameters: {"order"=>{"customer_name"=>"Jane Doe", "customer_phone"=>"+1 555 123 4567", "qty"=>2}}
[8f3a1c7e-5b2d] Completed 200 OK in 212ms (Views: 1.2ms | ActiveRecord: 3.4ms)
[1d2e3f40-aaaa] Completed 422 Unprocessable Content in 15ms (ActiveRecord: 0.9ms)
`)
	s.write("worker.log", `INFO arq.worker job started
ERROR judge Submission 77 failed
Traceback (most recent call last):
  File "/app/judge.py", line 10, in run
    raise RuntimeError("sandbox exploded")
RuntimeError: sandbox exploded
INFO arq.worker job finished
`)

	slow := s.waitFor("service:web-api @tag:api @ms:>200", 2)
	for _, l := range slow {
		if l.Attrs["tag"] != "api" || l.Service != "web-api" {
			t.Fatalf("%+v", l)
		}
	}

	rails := s.waitFor("service:orders-api @controller:OrdersController @duration:>200", 1)
	if rails[0].Message != "GET /orders 200" || rails[0].Attrs["action"] != "index" || rails[0].Attrs["status_code"] != float64(200) {
		t.Fatalf("%+v", rails[0])
	}
	if all := s.waitFor("service:orders-api @controller:OrdersController", 2); len(all) != 2 {
		t.Fatalf("interleaved requests were not grouped into two events: %+v", all)
	}

	errs := s.waitFor("service:worker status:error", 1)
	for _, want := range []string{"Submission 77 failed", "Traceback (most recent call last):", `File "/app/judge.py", line 10`, "RuntimeError: sandbox exploded"} {
		if !strings.Contains(errs[0].Message, want) {
			t.Fatalf("the traceback is not one event: %q lacks %q", errs[0].Message, want)
		}
	}
}

// The milestone's redaction acceptance criteria, as an explicit scan: run
// signup/login/reset-shaped logs through the whole path, then read EVERY stored
// log back and look for each secret, anywhere in its JSON.
func TestAcceptance_NoSecretIsFindableAfterTheSignupLoginResetFlows(t *testing.T) {
	s := startLogStack(t, e2eSources)
	const (
		jwt    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyLTQyIn0.Zm9vYmFyYmF6cXV4"
		reset  = "r3s3t-t0k3n-9f8e7d6c"
		bearer = "bearer-secret-abc123"
		phone  = "555 123 4567"
		email  = "jane.doe@example.test"
		pass   = "hunter2-correct-horse"
	)
	ms := s.clk.Now().UnixMilli()
	s.write("winston.log", fmt.Sprintf(`{"level":"info","message":"login","timestamp":%d,"user":%q,"password":%q,"token":%q,"headers":{"authorization":"Bearer %s","cookie":"sid=%s"}}
{"level":"info","message":"signup","timestamp":%d,"email":%q,"customer_phone":%q,"nested":{"api_key":"k-123456"}}
`, ms, email, pass, jwt, bearer, jwt, ms, email, "+1 "+phone))
	s.write("worker.log", fmt.Sprintf(`INFO app.auth verification link: https://app.example.test/verify?token=%s&lang=en for %s
INFO app.auth Authorization: Bearer %s
WARNING app.auth reset requested password=%s from %s
`, reset, email, jwt, pass, email))
	s.write("rails.log", fmt.Sprintf(`[aaaaaaaa-1111] Started POST "/orders?access_token=%s" for 1.1.1.1 at 2026-09-21 14:13:20 +0000
[aaaaaaaa-1111]   Parameters: {"order"=>{"customer_phone"=>"+1 %s", "customer_name"=>"Jane"}}
[aaaaaaaa-1111] Completed 201 Created in 20ms
`, bearer, phone))
	s.waitFor("login", 1)
	s.waitFor("signup", 1)
	s.waitFor("verification", 1)
	s.waitFor("Authorization", 1)
	s.waitFor("reset requested", 1)
	s.waitFor("service:orders-api", 1)

	// Read everything back: every stored log, whole.
	all := s.search("")
	if len(all) < 6 {
		t.Fatalf("only %d logs came back to scan", len(all))
	}
	dump, _ := json.Marshal(all)
	for name, secret := range map[string]string{
		"JWT": jwt, "reset token": reset, "bearer token": bearer, "phone": phone, "email": email, "password": pass, "api key": "k-123456",
		"jwt payload": "eyJzdWIiOiJ1c2VyLTQyIn0",
	} {
		if strings.Contains(string(dump), secret) {
			t.Errorf("%s is in the store: %s", name, secret)
		}
		// And by search, which is what a person would do: free text over message and attributes.
		if got := s.search(`"` + secret + `"`); len(got) != 0 {
			t.Errorf("%s is findable by search (%d logs): %s", name, len(got), secret)
		}
	}
	if !strings.Contains(string(dump), "[REDACTED]") {
		t.Error("nothing was redacted: the scan above proves nothing")
	}
	// What was not secret is still there.
	if got := s.search("lang=en"); len(got) != 1 {
		t.Errorf("the non-secret part of the reset link was lost: %d", len(got))
	}
}
