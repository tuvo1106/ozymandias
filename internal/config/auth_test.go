package config

import (
	"strings"
	"testing"
	"time"
)

func TestAuth_DefaultsOffAndValidates(t *testing.T) {
	c := DefaultOzyd()
	if c.Auth.Enabled {
		t.Fatal("auth must be off by default: existing deployments must not lock themselves out on upgrade")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Auth){
		"auth.session_ttl":                     func(a *Auth) { a.SessionTTL = -time.Second },
		"auth.defaults must not be negative":   func(a *Auth) { a.Defaults.RequestsPerSec = -1 },
		"auth.defaults must not be negativeB":  func(a *Auth) { a.Defaults.BytesPerSec = -1 },
		"auth.defaults must not be negativeS":  func(a *Auth) { a.Defaults.SpansPerSec = -1 },
		"auth.defaults must not be negativeLB": func(a *Auth) { a.Defaults.LogBytesPerDay = -1 },
	} {
		c := DefaultOzyd()
		mod(&c.Auth)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "auth.") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
