package collector

import (
	"errors"
	"fmt"
	"net/url"
)

// ParseCheckURL parses a check's target URL, which must be absolute http or
// https with a host: a relative URL or another scheme would fail on every
// run rather than at startup. Its error names the URL redacted
// ([RedactURL]), since startup errors are logged too. Every check that
// takes a URL uses it, so they accept the same URLs and hide the same
// parts.
func ParseCheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("url %s: want an absolute http or https URL", RedactURL(raw))
	}
	return u, nil
}

// RedactURL renders a URL for a log line or an error: the password becomes
// "xxxxx" (as url.URL.Redacted does) and the query and fragment are cut,
// leaving "?…" where a query was. A check's URL is configuration, and
// configuration carries secrets in both places — basic-auth passwords in
// the userinfo, API tokens in the query — while its errors end up in the
// agent's log and wherever that log is shipped. url.Redacted alone is not
// enough: it keeps the query. The scheme, host and path stay, which is
// what someone reading the log needs to know which target failed.
//
// Text that does not parse as a URL is replaced whole, since there is no
// telling which part of it is a secret.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	return redacted(u)
}

func redacted(u *url.URL) string {
	c := *u
	hadQuery := c.RawQuery != "" || c.ForceQuery
	c.RawQuery, c.ForceQuery, c.Fragment, c.RawFragment = "", false, "", ""
	s := c.Redacted()
	if hadQuery {
		s += "?…"
	}
	return s
}

// RedactURLError rewrites the URL inside every *url.Error in err's chain
// with [RedactURL]. net/http's client wraps its failures in one and
// strips only the password, so a request that fails after following a
// redirect to https://host/cb?token=… would otherwise print the token.
// The error is changed in place: it was made for this request alone.
//
// Call it on the error as the client returns it, before wrapping it:
// fmt.Errorf formats its message when it is made, so redacting the chain
// afterwards leaves the secret in the wrapper's text.
func RedactURLError(err error) error {
	var ue *url.Error
	for e := err; errors.As(e, &ue); e = ue.Err {
		ue.URL = RedactURL(ue.URL)
	}
	return err
}

// RequestError is err, a request to the check's URL failing, made fit for a
// message that already names that URL as shown ([RedactURL] of it): every
// URL in the chain is redacted, and the outermost *url.Error, which only
// repeats the method and URL, is dropped when its URL is the one shown. A
// request that failed elsewhere, after a redirect, keeps its wrapper, so
// the log says which URL failed — redacted like the rest.
func RequestError(shown string, err error) error {
	err = RedactURLError(err)
	// The outermost error only: a *url.Error deeper in the chain is inside
	// some other message, which cannot be unwrapped around it.
	if ue, ok := err.(*url.Error); ok && ue.URL == shown { //nolint:errorlint // outermost only, on purpose
		return ue.Err
	}
	return err
}
