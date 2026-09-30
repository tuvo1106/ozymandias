package httpcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// got is one run's output by metric name.
type got map[string]collector.Metric

func run(t *testing.T, c collector.Collector) (got, error) {
	t.Helper()
	out := got{}
	err := c.Collect(context.Background(), func(m collector.Metric) {
		if _, dup := out[m.Name]; dup {
			t.Errorf("%s emitted twice", m.Name)
		}
		out[m.Name] = m
	})
	return out, err
}

func (g got) value(t *testing.T, name string) float64 {
	t.Helper()
	m, ok := g[name]
	if !ok {
		t.Fatalf("%s not emitted; got %v", name, g)
	}
	return m.Value
}

// newCheck builds an instance through the registry, the way the agent does.
func newCheck(t *testing.T, settings map[string]any, clk *testutil.FakeClock) collector.Collector {
	t.Helper()
	reg := collector.Registry{Name: New}
	var c collector.Collector
	var err error
	if clk == nil {
		c, err = reg.NewInstance(Name, 0, 1, settings, nil, nil, slog.New(slog.DiscardHandler))
	} else {
		c, err = reg.NewInstance(Name, 0, 1, settings, nil, clk, slog.New(slog.DiscardHandler))
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheck_Up(t *testing.T) {
	fc := testutil.NewFakeClock(t0)
	var gotReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		fc.Advance(250 * time.Millisecond) // the server takes a quarter second
		_, _ = io.WriteString(w, "<html>all systems nominal</html>")
	}))
	defer srv.Close()
	c := newCheck(t, map[string]any{
		"url": srv.URL + "/health?token=secret", "method": "head", "timeout": "2s",
		"headers": map[string]any{"X-Probe": "ozy"},
	}, fc)
	g, err := run(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Method != http.MethodHead || gotReq.Header.Get("X-Probe") != "ozy" {
		t.Errorf("request %s with headers %v", gotReq.Method, gotReq.Header)
	}
	for name, want := range map[string]float64{
		"network.http.can_connect": 1, "network.http.up": 1,
		"network.http.status_code": 200, "network.http.response_time": 0.25,
	} {
		if v := g.value(t, name); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	if _, ok := g["network.http.ssl.days_left"]; ok {
		t.Error("days_left for plain http")
	}
	// The query string (a token) never reaches a tag.
	tags := g["network.http.up"].Tags
	if len(tags) != 1 || tags[0] != "url:"+strings.ToLower(srv.URL)+"/health" {
		t.Errorf("tags = %v", tags)
	}
}

func TestCheck_StatusAndContent(t *testing.T) {
	status, body := http.StatusOK, "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := newCheck(t, map[string]any{"url": srv.URL}, nil)
	status = http.StatusServiceUnavailable
	g, err := run(t, c)
	if err == nil || !strings.Contains(err.Error(), "status 503, want 2xx or 3xx") {
		t.Fatalf("err = %v", err)
	}
	if g.value(t, "network.http.up") != 0 || g.value(t, "network.http.can_connect") != 1 || g.value(t, "network.http.status_code") != 503 {
		t.Errorf("503: %v", g)
	}

	status = http.StatusNotFound // 4xx is down too, not just 5xx
	if g, err = run(t, c); err == nil || g.value(t, "network.http.up") != 0 {
		t.Fatalf("404: %v %v", g, err)
	}
	status = http.StatusServiceUnavailable

	c = newCheck(t, map[string]any{"url": srv.URL, "expected_status": []any{503}}, nil)
	if _, err := run(t, c); err != nil {
		t.Errorf("an expected 503: %v", err)
	}
	c = newCheck(t, map[string]any{"url": srv.URL, "expected_status": []any{200, 204}}, nil)
	if _, err := run(t, c); err == nil || !strings.Contains(err.Error(), "want 200 or 204") {
		t.Errorf("err = %v", err)
	}

	status = http.StatusOK
	c = newCheck(t, map[string]any{"url": srv.URL, "content_match": "^all good$"}, nil)
	g, err = run(t, c)
	if err == nil || !strings.Contains(err.Error(), "does not match") || g.value(t, "network.http.up") != 0 {
		t.Fatalf("content mismatch: %v %v", g, err)
	}
	body = "all good"
	if g, err = run(t, c); err != nil || g.value(t, "network.http.up") != 1 {
		t.Fatalf("content match: %v %v", g, err)
	}
}

func TestCheck_CannotConnect(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	c := newCheck(t, map[string]any{"url": "http://" + addr + "/"}, nil)
	g, err := run(t, c)
	if err == nil || strings.Count(err.Error(), "http://") != 1 {
		t.Fatalf("err = %v: want the URL named once", err)
	}
	if g.value(t, "network.http.can_connect") != 0 || g.value(t, "network.http.up") != 0 {
		t.Errorf("%v", g)
	}
	if _, ok := g["network.http.response_time"]; ok {
		t.Error("a response time without a response")
	}
}

func TestCheck_Timeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := newCheck(t, map[string]any{"url": srv.URL, "timeout": "50ms"}, nil)
	g, err := run(t, c)
	if !errors.Is(err, context.DeadlineExceeded) || g.value(t, "network.http.can_connect") != 0 {
		t.Fatalf("%v %v", g, err)
	}
}

func TestCheck_BodyCutShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer srv.Close()
	g, err := run(t, newCheck(t, map[string]any{"url": srv.URL}, nil))
	if err == nil || !strings.Contains(err.Error(), "reading the body") || g.value(t, "network.http.up") != 0 {
		t.Fatalf("%v %v", g, err)
	}
}

func TestCheck_Redirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/old", http.RedirectHandler("/new", http.StatusMovedPermanently))
	mux.HandleFunc("/new", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g, _ := run(t, newCheck(t, map[string]any{"url": srv.URL + "/old"}, nil))
	if g.value(t, "network.http.status_code") != 200 {
		t.Errorf("followed: %v", g)
	}
	g, _ = run(t, newCheck(t, map[string]any{"url": srv.URL + "/old", "follow_redirects": false}, nil))
	if g.value(t, "network.http.status_code") != 301 || g.value(t, "network.http.up") != 1 {
		t.Errorf("not followed: %v", g)
	}
}

func TestCheck_TLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted case's handshake error
	srv.StartTLS()
	defer srv.Close()
	leaf := srv.Certificate()
	fc := testutil.NewFakeClock(leaf.NotAfter.Add(-36 * time.Hour))

	// Trusted: the certificate's remaining life, on the injected clock.
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	c, err := build(Config{URL: srv.URL}, fc, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	g, err := run(t, c)
	if err != nil || g.value(t, "network.http.ssl.days_left") != 1.5 {
		t.Fatalf("%v %v", g, err)
	}

	// Untrusted: the connection fails, unless told not to verify.
	g, err = run(t, newCheck(t, map[string]any{"url": srv.URL}, fc))
	if err == nil || g.value(t, "network.http.can_connect") != 0 {
		t.Fatalf("untrusted certificate: %v %v", g, err)
	}
	g, err = run(t, newCheck(t, map[string]any{"url": srv.URL, "tls_skip_verify": true}, fc))
	if err != nil || g.value(t, "network.http.ssl.days_left") != 1.5 {
		t.Fatalf("tls_skip_verify: %v %v", g, err)
	}
}

// A Host header reaches the server whatever case the YAML key was written
// in: net/http ignores a Host header and sends req.Host.
func TestCheck_HostHeader(t *testing.T) {
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { host = r.Host }))
	defer srv.Close()
	for _, key := range []string{"Host", "host", "HOST"} {
		host = ""
		if _, err := run(t, newCheck(t, map[string]any{"url": srv.URL, "headers": map[string]any{key: "shop.test"}}, nil)); err != nil {
			t.Fatal(err)
		}
		if host != "shop.test" {
			t.Errorf("headers %s: Host = %q", key, host)
		}
	}
}

func TestNew_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		want     string
	}{
		"no url":           {map[string]any{}, "absolute http or https URL"},
		"relative url":     {map[string]any{"url": "/health"}, "absolute http or https URL"},
		"ftp":              {map[string]any{"url": "ftp://x/"}, "absolute http or https URL"},
		"bad status":       {map[string]any{"url": "http://x/", "expected_status": []any{700}}, "not an HTTP status"},
		"bad regexp":       {map[string]any{"url": "http://x/", "content_match": "("}, "content_match"},
		"negative timeout": {map[string]any{"url": "http://x/", "timeout": "-1s"}, "must be positive"},
		"misspelt setting": {map[string]any{"url": "http://x/", "methd": "GET"}, "methd"},
	} {
		_, err := collector.Registry{Name: New}.NewInstance(Name, 0, 1, tc.settings, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestNew_BadMethodIsARunError(t *testing.T) {
	g, err := run(t, newCheck(t, map[string]any{"url": "http://127.0.0.1/", "method": "BAD METHOD"}, nil))
	if err == nil || g.value(t, "network.http.up") != 0 {
		t.Fatalf("%v %v", g, err)
	}
}

// A URL that cannot be a tag is still checked, just without the tag.
func TestNew_UntaggableURL(t *testing.T) {
	c, err := build(Config{URL: "http://x/a,b"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.tags != nil {
		t.Errorf("tags = %v", c.tags)
	}
	if c.Name() != Name || c.Interval() != 0 {
		t.Errorf("%q %v", c.Name(), c.Interval())
	}
}

// Errors are logged, so the URL they name is redacted: a basic-auth
// password or a token in the query must not reach the log.
func TestCheck_ErrorsHideSecrets(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	_ = ln.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()
	// A redirect to a dead address: the client's error then names the
	// redirect target, which carries its own token.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://u:secret@"+closed+"/cb?token=abc", http.StatusFound)
	}))
	defer redirect.Close()
	for name, base := range map[string]string{
		"status": unavailable.URL, "dial": "http://" + closed, "redirect": redirect.URL,
	} {
		t.Run(name, func(t *testing.T) {
			u := strings.Replace(base, "http://", "http://admin:secret@", 1) + "/health?token=abc"
			c := newCheck(t, map[string]any{"url": u}, nil)
			_, err := run(t, c)
			if err == nil {
				t.Fatal("no error")
			}
			if msg := err.Error(); strings.Contains(msg, "secret") || strings.Contains(msg, "abc") {
				t.Fatalf("a secret reached the error: %s", msg)
			}
		})
	}
	_, err := New(collector.Instance{Name: Name, Settings: map[string]any{"url": "ftp://admin:secret@host/?token=abc"}})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "abc") {
		t.Fatalf("config error: %v", err)
	}
}
