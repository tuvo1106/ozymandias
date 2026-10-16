package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNotifierValidation(t *testing.T) {
	ok := map[string]Notifier{
		"hook":        {Type: "webhook", URL: "http://x"},
		"hook2":       {Type: "webhook", URLEnv: "X"},
		"discord-ops": {Type: "discord", WebhookURLEnv: "D"},
		"me":          {Type: "email", To: []string{"a@b"}, SMTP: SMTP{Host: "h", Port: 587, From: "f@l"}},
		"log":         {Type: "log"},
	}
	if err := validateNotifiers(ok); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		n    Notifier
		want string
	}{
		"bad name!": {Notifier{Type: "log"}, "a name is"},
		"both":      {Notifier{Type: "webhook", URL: "u", URLEnv: "E"}, "exactly one of url and url_env"},
		"neither":   {Notifier{Type: "webhook"}, "exactly one of url and url_env"},
		"dsecret":   {Notifier{Type: "discord", URL: "http://d"}, "webhook_url_env"},
		"noto":      {Notifier{Type: "email", SMTP: SMTP{Host: "h", Port: 25, From: "f"}}, "to"},
		"nohost":    {Notifier{Type: "email", To: []string{"a"}, SMTP: SMTP{Port: 25, From: "f"}}, "host"},
		"port":      {Notifier{Type: "email", To: []string{"a"}, SMTP: SMTP{Host: "h", From: "f"}}, "port"},
		"from":      {Notifier{Type: "email", To: []string{"a"}, SMTP: SMTP{Host: "h", Port: 25}}, "from"},
		"pager":     {Notifier{Type: "pager"}, "want webhook"},
	} {
		err := validateNotifiers(map[string]Notifier{name: c.n})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestMonitorsValidation(t *testing.T) {
	m := DefaultOzyd().Monitors
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mod  func(*Monitors)
		want string
	}{
		{func(m *Monitors) { m.Workers = 0 }, "workers"},
		{func(m *Monitors) { m.EventRetention = 0 }, "event_retention"},
		{func(m *Monitors) { m.BaseURL = "localhost:9400" }, "base_url"},
	} {
		x := m
		c.mod(&x)
		if err := x.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v, want %q", err, c.want)
		}
	}
	x := m
	x.EventRetention = -time.Hour
	if err := x.Validate(); err != nil {
		t.Errorf("negative retention: %v", err)
	}
}

// TestNotifiersFromYAML: the map decodes strictly, so a misspelt key in one
// notifier is a startup error, not a notifier that silently lacks a field.
func TestNotifiersFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := dir + "/ozyd.yaml"
		if err := writeFile(p, body); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, _, err := LoadOzyd(write(`
notifiers:
  discord-ops: {type: discord, webhook_url_env: DISCORD_WEBHOOK_URL}
  me: {type: email, to: [a@b.c], smtp: {host_env: H, port: 587, from: o@l}}
provisioning:
  monitor_paths: [./deploy/monitors]
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifiers["discord-ops"].WebhookURLEnv != "DISCORD_WEBHOOK_URL" || cfg.Notifiers["me"].SMTP.Port != 587 ||
		cfg.Provisioning.MonitorPaths[0] != "./deploy/monitors" {
		t.Fatalf("%+v", cfg.Notifiers)
	}
	if _, _, err := LoadOzyd(write("notifiers:\n  x: {type: discord, webhook_url: http://leak}\n"), nil); err == nil {
		t.Fatal("unknown notifier key accepted")
	}
	if _, _, err := LoadOzyd(write("provisioning:\n  monitor_paths: ['']\n"), nil); err == nil {
		t.Fatal("empty monitor path accepted")
	}
}

func writeFile(p, body string) error { return os.WriteFile(p, []byte(body), 0o644) }
