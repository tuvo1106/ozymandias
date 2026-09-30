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

// Every check's URL is held to one rule: absolute http(s) with a host, and
// the refusal names it redacted.
func TestParseCheckURL(t *testing.T) {
	if u, err := ParseCheckURL("https://h:1/p?q=1"); err != nil || u.Host != "h:1" {
		t.Fatalf("a good URL: %v %v", u, err)
	}
	for _, bad := range []string{"", "/metrics", "ftp://h/x", "http://", "localhost:9090/metrics", "http://u:s3cret@/x?token=abc"} {
		_, err := ParseCheckURL(bad)
		if err == nil {
			t.Errorf("%q was accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "abc") {
			t.Errorf("%q: the refusal leaks: %v", bad, err)
		}
	}
}

// A failure at the URL the message already names is not named twice; one
// at another URL (after a redirect) keeps it, redacted.
func TestRequestError(t *testing.T) {
	shown := RedactURL("http://u:p@host/x?token=abc")
	inner := errors.New("connection refused")
	here := &url.Error{Op: "Get", URL: "http://u:p@host/x?token=abc", Err: inner}
	if got := RequestError(shown, here); got.Error() != inner.Error() {
		t.Fatalf("at the named URL: %v", got)
	}
	there := &url.Error{Op: "Get", URL: "http://other/cb?token=xyz", Err: inner}
	got := RequestError(shown, there).Error()
	if !strings.Contains(got, "http://other/cb?…") || strings.Contains(got, "xyz") {
		t.Fatalf("after a redirect: %q", got)
	}
	if plain := errors.New("x"); !errors.Is(RequestError(shown, plain), plain) {
		t.Fatal("a non-URL error changed")
	}
}
