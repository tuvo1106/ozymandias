package collector

import (
	"errors"
	"net/url"
)

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
