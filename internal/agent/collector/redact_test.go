package collector

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://host:8443/health", "https://host:8443/health"},
		{"https://admin:s3cret@host/health", "https://admin:xxxxx@host/health"},
		{"http://host/metrics?token=abc&x=1", "http://host/metrics?…"},
		{"http://u:p@host/p?token=abc#frag", "http://u:xxxxx@host/p?…"},
		{"http://host/p#secret", "http://host/p"},
		{"http://host/p?", "http://host/p?…"},
		{"http://a b\x7f", "(unparseable URL)"},
	} {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactURLError(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://u:s3cret@host/cb?token=abc", Err: errors.New("boom")}
	err := fmt.Errorf("scrape: %w", RedactURLError(inner))
	msg := err.Error()
	if strings.Contains(msg, "s3cret") || strings.Contains(msg, "abc") {
		t.Fatalf("a secret survived: %s", msg)
	}
	if !strings.Contains(msg, "https://u:xxxxx@host/cb?…") || !errors.Is(err, inner) {
		t.Fatalf("the error lost its shape: %s", msg)
	}
	if RedactURLError(nil) != nil {
		t.Fatal("nil is not nil")
	}
}
