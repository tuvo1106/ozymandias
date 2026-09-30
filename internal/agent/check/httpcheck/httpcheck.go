package httpcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Name is the check's registered name.
const Name = "http_check"

// Defaults for [Config].
const (
	DefaultMethod  = http.MethodGet
	DefaultTimeout = 5 * time.Second
	// maxBody is how much of the body is read: enough for content_match to
	// find a marker on a normal page, bounded so a check pointed at a large
	// download does not pull it every 15 seconds.
	maxBody = 64 << 10
)

// Config is one instance's settings.
type Config struct {
	URL    string `yaml:"url"`
	Method string `yaml:"method"`
	// Timeout bounds the whole request, body included. Default 5s.
	Timeout time.Duration `yaml:"timeout"`
	// ExpectedStatus lists the statuses that count as up. Empty means any
	// 2xx or 3xx.
	ExpectedStatus []int `yaml:"expected_status"`
	// ContentMatch, if set, is a regular expression the first 64 KiB of the
	// body must match.
	ContentMatch string `yaml:"content_match"`
	// TLSSkipVerify accepts any certificate. days_left is still reported,
	// from whatever certificate the server sent.
	TLSSkipVerify bool              `yaml:"tls_skip_verify"`
	Headers       map[string]string `yaml:"headers"`
	// FollowRedirects follows up to ten redirects (default true); false
	// reports the redirect itself, e.g. a 301 from http to https.
	FollowRedirects *bool `yaml:"follow_redirects"`
}

// Check is one http_check instance.
type Check struct {
	cfg     Config
	content *regexp.Regexp
	client  *http.Client
	clock   clock.Clock
	tags    []string
	// shown is the URL as errors print it: redacted, since they are
	// logged ([collector.RedactURL]).
	shown string
}

var _ collector.Collector = (*Check)(nil)

// New is the http_check factory.
func New(inst collector.Instance) (collector.Collector, error) {
	var cfg Config
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	return build(cfg, inst.Clock, nil)
}

// build validates cfg and makes the check. tlsBase, if not nil, is the TLS
// configuration to start from (tests pass one trusting their server).
func build(cfg Config, clk clock.Clock, tlsBase *tls.Config) (*Check, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("url %s: want an absolute http or https URL", collector.RedactURL(cfg.URL))
	}
	if cfg.Method == "" {
		cfg.Method = DefaultMethod
	}
	cfg.Method = strings.ToUpper(cfg.Method)
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("timeout %v must be positive", cfg.Timeout)
	}
	for _, s := range cfg.ExpectedStatus {
		if s < 100 || s > 599 {
			return nil, fmt.Errorf("expected_status %d is not an HTTP status", s)
		}
	}
	c := &Check{cfg: cfg, clock: clk, shown: collector.RedactURL(cfg.URL)}
	if c.clock == nil {
		c.clock = clock.Real()
	}
	if cfg.ContentMatch != "" {
		if c.content, err = regexp.Compile(cfg.ContentMatch); err != nil {
			return nil, fmt.Errorf("content_match: %w", err)
		}
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if tlsBase != nil {
		tc = tlsBase.Clone()
	}
	tc.InsecureSkipVerify = cfg.TLSSkipVerify //nolint:gosec // an explicit, documented setting
	tr := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tc,
		// A fresh connection each run: a check that reused one would
		// measure a request, not what a new client waits for, and would
		// not notice a listener that stopped accepting.
		DisableKeepAlives: true,
	}
	c.client = &http.Client{Transport: tr}
	if cfg.FollowRedirects != nil && !*cfg.FollowRedirects {
		c.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	if tag, ok := wire.NormalizeTag("url:" + (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()); ok {
		c.tags = []string{tag}
	}
	return c, nil
}

// Name implements [collector.Collector]; the instance wrapper names it.
func (c *Check) Name() string { return Name }

// Interval implements [collector.Collector]: the scheduler's default.
func (c *Check) Interval() time.Duration { return 0 }

// Collect makes one request and reports on it.
func (c *Check) Collect(ctx context.Context, emit collector.Emit) error {
	gauge := func(name string, v float64) {
		emit(collector.Metric{Name: name, Kind: collector.Gauge, Value: v, Tags: c.tags})
	}
	down := func(canConnect float64, err error) error {
		gauge("network.http.can_connect", canConnect)
		gauge("network.http.up", 0)
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, c.cfg.Method, c.cfg.URL, nil)
	if err != nil {
		return down(0, fmt.Errorf("%s: %w", c.shown, unwrapURL(err)))
	}
	for k, v := range c.cfg.Headers {
		// net/http sends req.Host, never a Host header, so the override has
		// to go there. Header names are case-insensitive and YAML keys are
		// whatever was typed: host, HOST and Host all mean it.
		if http.CanonicalHeaderKey(k) == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	start := c.clock.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		return down(0, fmt.Errorf("%s: %w", c.shown, unwrapURL(err)))
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	elapsed := c.clock.Now().Sub(start)
	if readErr != nil {
		return down(0, fmt.Errorf("%s: reading the body: %w", c.shown, readErr))
	}

	gauge("network.http.can_connect", 1)
	gauge("network.http.response_time", elapsed.Seconds())
	gauge("network.http.status_code", float64(resp.StatusCode))
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		left := resp.TLS.PeerCertificates[0].NotAfter.Sub(c.clock.Now())
		gauge("network.http.ssl.days_left", left.Hours()/24)
	}

	var why error
	switch {
	case !c.statusOK(resp.StatusCode):
		why = fmt.Errorf("%s: status %d, want %s", c.shown, resp.StatusCode, c.wantStatus())
	case c.content != nil && !c.content.Match(body):
		why = fmt.Errorf("%s: the body does not match %q", c.shown, c.cfg.ContentMatch)
	}
	if why != nil {
		gauge("network.http.up", 0)
		return why
	}
	gauge("network.http.up", 1)
	return nil
}

func (c *Check) statusOK(code int) bool {
	if len(c.cfg.ExpectedStatus) == 0 {
		return code >= 200 && code < 400
	}
	for _, s := range c.cfg.ExpectedStatus {
		if s == code {
			return true
		}
	}
	return false
}

func (c *Check) wantStatus() string {
	if len(c.cfg.ExpectedStatus) == 0 {
		return "2xx or 3xx"
	}
	s := make([]string, len(c.cfg.ExpectedStatus))
	for i, n := range c.cfg.ExpectedStatus {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, " or ")
}

// unwrapURL drops the *url.Error wrapper, whose message repeats the method
// and URL the caller already names — and names unredacted: net/http strips
// the password from it but keeps the query.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
