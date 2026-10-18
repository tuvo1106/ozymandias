package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func sample() Notification {
	return Notification{
		MonitorID: 7, MonitorUID: "jobs-backlog", MonitorName: "Job queue is backing up", Tags: []string{"team:me"},
		GroupKey: "queue:default", Group: map[string]string{"queue": "default"},
		Status: "ALERT", PrevStatus: "WARN", Value: ValuePtr(25.5), Threshold: 20, Reason: "transition",
		Message: "Queue depth is 25.5 for queue:default. Check worker health. @hook",
		Link:    "http://localhost:9400/monitors/7?from=1",
		Ts:      t0,
	}
}

// golden compares got against testdata/<name>, rewriting it with -update-golden.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-golden to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed:\n got: %s\nwant: %s", name, got, want)
	}
}

func indent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestPayloadGoldens(t *testing.T) {
	golden(t, "webhook.json", indent(t, sample()))
	golden(t, "discord.json", indent(t, DiscordBody(sample())))
	nd := sample()
	nd.Status, nd.PrevStatus, nd.Value = "NO_DATA", "ALERT", nil
	golden(t, "webhook-nodata.json", indent(t, nd))
	golden(t, "discord-nodata.json", indent(t, DiscordBody(nd)))
	msg, err := EmailMessage("ozymandias@localhost", []string{"you@example.com"}, sample())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "email.eml", msg)
}

func TestTitle(t *testing.T) {
	n := sample()
	if got := n.Title(); got != "[ALERT] Job queue is backing up on queue:default" {
		t.Error(got)
	}
	n.Status, n.GroupKey = "OK", "*"
	if got := n.Title(); got != "[Recovered] Job queue is backing up" {
		t.Error(got)
	}
	n.Reason = "test"
	if got := n.Title(); !strings.HasPrefix(got, "[Test]") {
		t.Error(got)
	}
}

func TestDiscordTruncates(t *testing.T) {
	n := sample()
	n.MonitorName = strings.Repeat("é", 400)
	n.Message = strings.Repeat("x", 5000)
	b := DiscordBody(n).(discordBody)
	if r := []rune(b.Embeds[0].Title); len(r) != discordTitleMax || r[len(r)-1] != '…' {
		t.Errorf("title %d runes", len(r))
	}
	if r := []rune(b.Embeds[0].Description); len(r) != discordDescMax {
		t.Errorf("description %d runes", len(r))
	}
}

// TestEmailHeaderInjection: a monitor name carrying CRLF cannot add a header.
func TestEmailHeaderInjection(t *testing.T) {
	n := sample()
	n.MonitorName = "x\r\nBcc: attacker@example.com"
	msg, err := EmailMessage("a@localhost", []string{"b@example.com"}, n)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(msg), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("injected header: %q", head)
		}
	}
	if _, err := EmailMessage("not an address", []string{"b@example.com"}, n); err == nil {
		t.Error("bad from accepted")
	}
	if _, err := EmailMessage("a@localhost", nil, n); err == nil {
		t.Error("no recipients accepted")
	}
}

type capture struct {
	mu     sync.Mutex
	bodies [][]byte
	header http.Header
	status []int // served in order; last repeats
}

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
	c.header = r.Header.Clone()
	code := http.StatusNoContent
	if len(c.status) > 0 {
		i := min(len(c.bodies)-1, len(c.status)-1)
		code = c.status[i]
	}
	w.WriteHeader(code)
	_, _ = w.Write([]byte("nope"))
}

func TestWebhookAndDiscordDeliver(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	w := &Webhook{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer x"}}
	if err := w.Notify(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	var got Notification
	if err := json.Unmarshal(c.bodies[0], &got); err != nil || got.MonitorID != 7 || *got.Value != 25.5 {
		t.Fatalf("webhook body %s", c.bodies[0])
	}
	if c.header.Get("Authorization") != "Bearer x" || c.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", c.header)
	}
	if err := (&Discord{URL: srv.URL}).Notify(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(c.bodies[1], []byte(`"embeds"`)) {
		t.Fatalf("discord body %s", c.bodies[1])
	}
}

func TestHTTPErrorsClassify(t *testing.T) {
	for _, c := range []struct {
		code      int
		permanent bool
	}{{400, true}, {401, true}, {404, true}, {429, false}, {500, false}, {503, false}} {
		srv := httptest.NewServer(&capture{status: []int{c.code}})
		err := (&Webhook{URL: srv.URL}).Notify(context.Background(), sample())
		srv.Close()
		if err == nil || IsPermanent(err) != c.permanent || !strings.Contains(err.Error(), "nope") {
			t.Errorf("%d: %v (permanent %v)", c.code, err, IsPermanent(err))
		}
	}
}

// TestSecretURLNotInErrors: a Discord URL is the credential, and a
// connection error must not print it into an event anyone can read.
func TestSecretURLNotInErrors(t *testing.T) {
	secret := "http://127.0.0.1:1/api/webhooks/123/SECRET-TOKEN"
	err := (&Discord{URL: secret}).Notify(context.Background(), sample())
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("error %v", err)
	}
}

func TestBuild(t *testing.T) {
	env := map[string]string{"HOOK_URL": "http://h", "TOKEN": "t0k", "DISCORD": "http://d", "SMTP_HOST": "smtp.secret.host"}
	getenv := func(k string) string { return env[k] }
	ns, infos := Build(map[string]config.Notifier{
		"hook":    {Type: "webhook", URLEnv: "HOOK_URL", HeadersEnv: map[string]string{"Authorization": "TOKEN"}, Headers: map[string]string{"X": "y"}},
		"plain":   {Type: "webhook", URL: "http://p"},
		"ops":     {Type: "discord", WebhookURLEnv: "DISCORD"},
		"missing": {Type: "discord", WebhookURLEnv: "UNSET_VAR"},
		"me":      {Type: "email", To: []string{"a@b.c"}, SMTP: config.SMTP{HostEnv: "SMTP_HOST", Port: 25, From: "o@l"}},
		"nopass":  {Type: "email", To: []string{"a@b.c"}, SMTP: config.SMTP{Host: "h", Port: 25, From: "o@l", UserEnv: "SMTP_HOST"}},
		"log":     {Type: "log"},
	}, getenv, nil)
	if h := ns["hook"].(*Webhook); h.URL != "http://h" || h.Headers["Authorization"] != "t0k" || h.Headers["X"] != "y" {
		t.Errorf("hook %+v", h)
	}
	if e := ns["me"].(*Email); e.Host != "smtp.secret.host" || e.Port != 25 {
		t.Errorf("email %+v", e)
	}
	if _, ok := ns["missing"].(*Broken); !ok {
		t.Errorf("missing secret built %T", ns["missing"])
	}
	if _, ok := ns["nopass"].(*Broken); !ok {
		t.Errorf("user without password built %T", ns["nopass"])
	}
	err := ns["missing"].Notify(context.Background(), sample())
	if !IsPermanent(err) || !strings.Contains(err.Error(), "UNSET_VAR") {
		t.Errorf("broken notify %v", err)
	}
	// Infos carry names, types and the variable name; never a value.
	b, _ := json.Marshal(infos)
	for _, secret := range []string{"http://h", "t0k", "http://d", "smtp.secret.host"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("info leaks %q: %s", secret, b)
		}
	}
	if len(infos) != 7 || infos[0].Name != "hook" || !infos[0].Ready {
		t.Errorf("infos %s", b)
	}
	if err := ns["log"].Notify(context.Background(), sample()); err != nil {
		t.Error(err)
	}
}

func TestPermanentUnwraps(t *testing.T) {
	base := errors.New("x")
	if !errors.Is(&Permanent{base}, base) || IsPermanent(base) {
		t.Fatal("Permanent does not wrap")
	}
}
