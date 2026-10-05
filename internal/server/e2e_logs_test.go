package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// The log path in one process, every layer production code: a line appended
// to a file is tailed by a real agent, run through its pipeline, posted to a
// real ozyd, stored, and found by the search API; a live tail open at the time
// sees it; and a secret in it never reaches the store.
func TestEndToEnd_FileToSearchAndTail(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(1790000000, 0))
	srv, err := New(testConfig(t), Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer func() { _ = srv.Close() }()

	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "web.log")
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	acfg := agentconfig.Default()
	acfg.Hostname = "box"
	acfg.Tags = []string{"env:e2e"}
	acfg.Intake.URL = ts.URL
	acfg.Statsd.Enabled = false
	acfg.Collectors.Docker.Enabled = false
	acfg.Collectors.Host.Enabled = false
	acfg.HTTP.ShutdownTimeout = time.Second
	acfg.Logs.Enabled = true
	acfg.Logs.Sources = []agentconfig.LogSource{{
		Type: "file", Path: filepath.Join(logDir, "*.log"), Service: "web-api", StartPosition: "beginning",
		Spec: logpipeline.Spec{Source: "winston", RateLimit: -1},
	}}
	a, err := agent.New(acfg, agent.Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	defer func() { cancel(); <-done }()
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "log poller not started")

	// A live tail, opened before the line exists.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/logs/tail?q="+url.QueryEscape("status:error"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); !strings.HasPrefix(line, ":") {
		t.Fatalf("first line of the stream %q", line)
	}
	_, _ = br.ReadString('\n')

	appendLine := func(s string) {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s + "\n")
		_ = f.Close()
	}
	ms := func(offset time.Duration) int64 { return clk.Now().Add(offset).UnixMilli() }
	appendLine(fmt.Sprintf(`{"level":"info","message":"signed in","timestamp":%d,"user":"jane@example.test","token":"s3cr3t","route":"/login"}`, ms(0)))
	appendLine(fmt.Sprintf(`{"level":"error","message":"db down","timestamp":%d,"status":503,"route":"/orders"}`, ms(0)))

	search := func(q url.Values) map[string]any {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/logs?"+q.Encode(), nil))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	// Each attempt is one poll tick: a single Advance can land before the poller has
	// re-armed its timer on a loaded machine, and then nothing would ever tick again.
	testutil.Eventually(t, 5*time.Second, func() bool {
		clk.Advance(time.Second)
		return len(search(url.Values{"q": {"service:web-api"}})["logs"].([]any)) == 2
	}, "the two lines were not searchable")

	got := search(url.Values{"q": {"status:error @route:/orders"}})["logs"].([]any)
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	l := got[0].(map[string]any)
	attrs := l["attrs"].(map[string]any)
	if l["service"] != "web-api" || l["host"] != "box" || l["source"] != "winston" || attrs["status_code"] != float64(503) {
		t.Fatalf("%v", l)
	}
	if tags := fmt.Sprint(l["tags"]); !strings.Contains(tags, "env:e2e") {
		t.Fatalf("agent tags missing: %v", l["tags"])
	}

	// The secret and the email never reached the store.
	all, _ := json.Marshal(search(url.Values{"q": {"signed"}}))
	if strings.Contains(string(all), "s3cr3t") || strings.Contains(string(all), "jane@example.test") {
		t.Fatalf("a secret reached the store: %s", all)
	}
	if !strings.Contains(string(all), "[REDACTED]") {
		t.Fatalf("expected redaction markers: %s", all)
	}

	// The tail saw the error log, and only it.
	ev, _ := br.ReadString('\n')
	data, _ := br.ReadString('\n')
	if strings.TrimSpace(ev) != "event: log" || !strings.Contains(data, "db down") || strings.Contains(data, "signed in") {
		t.Fatalf("tail got %q %q", ev, data)
	}
}
