package config

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveHostname(t *testing.T) {
	os := func(name string, err error) func() (string, error) {
		return func() (string, error) { return name, err }
	}
	for _, tc := range []struct {
		name, configured string
		lookup           func() (string, error)
		wantName         string
		wantTag          string // "" means an error is wanted
	}{
		{"configured wins", "Mac-Mini", os("0a1b2c", nil), "Mac-Mini", "host:mac-mini"},
		{"empty uses the OS", "", os("0a1b2c", nil), "0a1b2c", "host:0a1b2c"},
		{"OS lookup fails", "", os("", errors.New("no")), "", ""},
		{"OS gives nothing", "", os("", nil), "", ""},
		// The empty tag this used to produce made the intake drop every
		// self-metric series, every interval, with only a warning.
		{"a comma cannot be a tag", "mac,mini", nil, "", ""},
		{"too long for a tag", strings.Repeat("h", 200), nil, "", ""},
		{"a bad OS name is refused too", "", os("a,b", nil), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, tag, err := ResolveHostname(tc.configured, tc.lookup)
			if tc.wantTag == "" {
				if err == nil {
					t.Fatalf("got %q, %q; want an error", name, tag)
				}
				return
			}
			if err != nil || name != tc.wantName || tag != tc.wantTag {
				t.Fatalf("got %q, %q, %v; want %q, %q", name, tag, err, tc.wantName, tc.wantTag)
			}
		})
	}
}

func TestLoadOzyd_Hostname(t *testing.T) {
	// OZY_HOSTNAME is what `make up` and `make dev` set; renaming the YAML
	// key would silently break it, since unknown OZY_* variables only warn.
	cfg, warnings, err := LoadOzyd("", []string{"OZY_HOSTNAME=Mac-Mini"})
	if err != nil || len(warnings) != 0 || cfg.Hostname != "Mac-Mini" {
		t.Fatalf("hostname = %q, warnings %v, err %v", cfg.Hostname, warnings, err)
	}
	// A name that cannot be a tag is a config error (exit 2), not a
	// running ozyd whose every self-metric is refused.
	if _, _, err := LoadOzyd("", []string{"OZY_HOSTNAME=mac,mini"}); err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("OZY_HOSTNAME=mac,mini: err = %v, want a hostname error", err)
	}
}
