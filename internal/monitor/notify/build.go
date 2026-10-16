package notify

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/tuvo1106/ozymandias/internal/config"
)

// Info describes a configured notifier for the API: its name and type, and
// whether it could be built. Deliberately nothing else — no URL, no address,
// no header — so that listing notifiers can never be how a secret leaks.
type Info struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Ready bool   `json:"ready"`
	// Problem says why a notifier is not ready, naming the environment
	// variable to set, never its value.
	Problem string `json:"problem,omitempty"`
}

// Build makes the configured notifiers, resolving every *_env field through
// getenv. A notifier whose variable is unset becomes a [Broken] one: present,
// mentionable, and failing each delivery with a reason that names the
// variable, so the operator finds out from the monitor's event log instead of
// from a page that never came.
func Build(cfgs map[string]config.Notifier, getenv func(string) string, log *slog.Logger) (map[string]Notifier, []Info) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	hc := &http.Client{Timeout: DefaultTimeout}
	out := map[string]Notifier{}
	var infos []Info
	for name, c := range cfgs {
		n, err := build(c, getenv, hc, log)
		info := Info{Name: name, Type: c.Type, Ready: err == nil}
		if err != nil {
			info.Problem = err.Error()
			n = &Broken{Reason: err}
			log.Warn("notifier is not usable until its configuration is fixed", "notifier", name, "problem", err)
		}
		out[name] = n
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return out, infos
}

func build(c config.Notifier, getenv func(string) string, hc *http.Client, log *slog.Logger) (Notifier, error) {
	env := func(name string) (string, error) {
		v := getenv(name)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		return v, nil
	}
	switch c.Type {
	case "webhook":
		u := c.URL
		if c.URLEnv != "" {
			var err error
			if u, err = env(c.URLEnv); err != nil {
				return nil, err
			}
		}
		h := map[string]string{}
		for k, v := range c.Headers {
			h[k] = v
		}
		for k, name := range c.HeadersEnv {
			v, err := env(name)
			if err != nil {
				return nil, err
			}
			h[k] = v
		}
		return &Webhook{URL: u, Headers: h, Client: hc}, nil
	case "discord":
		u, err := env(c.WebhookURLEnv)
		if err != nil {
			return nil, err
		}
		return &Discord{URL: u, Client: hc}, nil
	case "email":
		host := c.SMTP.Host
		if c.SMTP.HostEnv != "" {
			var err error
			if host, err = env(c.SMTP.HostEnv); err != nil {
				return nil, err
			}
		}
		e := &Email{Host: host, Port: c.SMTP.Port, From: c.SMTP.From, To: c.To}
		if c.SMTP.UserEnv != "" {
			var err error
			if e.User, err = env(c.SMTP.UserEnv); err != nil {
				return nil, err
			}
			if c.SMTP.PasswordEnv == "" {
				return nil, fmt.Errorf("smtp.user_env is set but smtp.password_env is not")
			}
			if e.Password, err = env(c.SMTP.PasswordEnv); err != nil {
				return nil, err
			}
		}
		return e, nil
	case "log":
		return &Log{Logger: log}, nil
	}
	return nil, fmt.Errorf("unknown notifier type %q", c.Type)
}
