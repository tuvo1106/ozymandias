package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// ResolveHostname is the one place ozyd and the agent decide what to put in
// their `host` tag: the configured name if there is one, else lookup (the OS
// hostname). It returns the name as given and the normalized `host:<name>`
// tag.
//
// A name the wire cannot carry as a tag ("a,b", or one past the length
// limit) is an error rather than an empty tag. An empty tag used to be the
// result, and the intake then rejected every self-metric series every
// interval with nothing but a warning in the log.
func ResolveHostname(configured string, lookup func() (string, error)) (name, tag string, err error) {
	name = configured
	if name == "" {
		if name, err = lookup(); err != nil {
			return "", "", fmt.Errorf("hostname is not configured and the OS hostname is unavailable: %w", err)
		}
		if name == "" {
			return "", "", errors.New("hostname is not configured and the OS hostname is empty")
		}
	}
	// NormalizeTag keeps surrounding spaces and drops a trailing ':', so
	// these would make a valid tag that is not the name: "mac " would be a
	// second host value beside "mac", and "mac:" would be reported as one
	// name and tagged as another.
	if strings.TrimSpace(name) != name || strings.HasSuffix(name, ":") {
		return "", "", fmt.Errorf("hostname %q has surrounding whitespace or a trailing ':'", name)
	}
	tag, ok := wire.NormalizeTag("host:" + name)
	if !ok {
		return "", "", fmt.Errorf("hostname %q cannot be a host tag (no commas; at most %d bytes as host:<name>)", name, wire.MaxTagLen)
	}
	return name, tag, nil
}

// ValidateHostname is the config-time half of [ResolveHostname]: a
// configured name must make a valid tag, so a typo fails startup with exit 2
// like any other config error. Empty is valid; it means the OS hostname.
func ValidateHostname(h string) error {
	if h == "" {
		return nil
	}
	_, _, err := ResolveHostname(h, nil)
	return err
}
