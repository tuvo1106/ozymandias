package openmetrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	om "github.com/tuvo1106/ozymandias/internal/agent/collector/openmetrics"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/sketch"
)

// Defaults for [Config].
const (
	DefaultTimeout   = 10 * time.Second
	DefaultMaxBody   = 10 << 20
	DefaultMaxSeries = 2000
)

// Config is the check's settings (collectors.checks.openmetrics.instances[]).
type Config struct {
	// URL is the page to scrape, http or https.
	URL string `yaml:"url"`
	// Namespace, when set, is prefixed to every metric name with a dot.
	Namespace string `yaml:"namespace"`
	// Metrics are regular expressions; a metric is kept when one matches its
	// name (see the package doc for which name). Empty keeps everything.
	Metrics []string `yaml:"metrics"`
	// Exclude are regular expressions; a metric whose name matches one is
	// dropped, even if Metrics kept it.
	Exclude []string `yaml:"exclude"`
	// Rename maps a metric name to the name to send it under.
	Rename map[string]string `yaml:"rename"`
	// ExcludeLabels are labels not turned into tags.
	ExcludeLabels []string `yaml:"exclude_labels"`
	// HistogramBucketsAsDistributions sends each histogram as a
	// distribution — the interval's bucket counts spread into a DDSketch —
	// instead of per-bucket counts, so p50:/p99: work on it as on a statsd
	// distribution. Lossy: a value is known only to its bucket.
	HistogramBucketsAsDistributions bool `yaml:"histogram_buckets_as_distributions"`
	// Timeout bounds one scrape. Default 10s; the scheduler's run timeout
	// also applies.
	Timeout time.Duration `yaml:"timeout"`
	// MaxBody bounds the page, in bytes. Default 10 MiB.
	MaxBody int64 `yaml:"max_body"`
	// MaxSeries bounds the metrics one scrape emits. Default 2000.
	MaxSeries int `yaml:"max_series"`
}

// Check scrapes one target.
type Check struct {
	cfg           Config
	url           string
	allow, deny   []*regexp.Regexp
	excludeLabels map[string]bool
	client        *http.Client
	clock         clock.Clock
	log           *slog.Logger

	rates   *collector.Rates
	counts  *deltas
	buckets map[string]bucketState
	swept   time.Time
}

// bucketState is one histogram series' previous cumulative buckets.
type bucketState struct {
	buckets []om.Bucket
	at      time.Time
}

// New builds the check from one instance's settings.
func New(inst collector.Instance) (collector.Collector, error) {
	var cfg Config
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	return newCheck(cfg, inst.Clock, inst.Logger)
}

func newCheck(cfg Config, clk clock.Clock, log *slog.Logger) (*Check, error) {
	u, err := url.Parse(cfg.URL)
	if cfg.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("url %q: want an absolute http(s) URL", cfg.URL)
	}
	if cfg.Timeout < 0 || cfg.MaxBody < 0 || cfg.MaxSeries < 0 {
		return nil, errors.New("timeout, max_body and max_series must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if cfg.MaxSeries == 0 {
		cfg.MaxSeries = DefaultMaxSeries
	}
	c := &Check{
		cfg: cfg, url: cfg.URL, excludeLabels: map[string]bool{},
		client: &http.Client{Timeout: cfg.Timeout}, clock: clk, log: log,
		rates: collector.NewRates(), counts: newDeltas(), buckets: map[string]bucketState{},
	}
	if c.clock == nil {
		c.clock = clock.Real()
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	for _, list := range []struct {
		key  string
		res  []string
		into *[]*regexp.Regexp
	}{{"metrics", cfg.Metrics, &c.allow}, {"exclude", cfg.Exclude, &c.deny}} {
		for _, re := range list.res {
			rx, err := regexp.Compile(re)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", list.key, re, err)
			}
			*list.into = append(*list.into, rx)
		}
	}
	for _, l := range cfg.ExcludeLabels {
		c.excludeLabels[l] = true
	}
	return c, nil
}

// Name implements [collector.Collector]; the instance wrapper names it.
func (c *Check) Name() string { return "openmetrics" }

// Interval implements [collector.Collector]: the scheduler's default.
func (c *Check) Interval() time.Duration { return 0 }

// Collect scrapes once. up and scrape_duration are emitted whatever
// happens; a failed scrape then returns its error.
func (c *Check) Collect(ctx context.Context, emit collector.Emit) error {
	start := c.clock.Now()
	fams, err := c.scrape(ctx)
	emit(collector.Metric{Name: "openmetrics.scrape_duration", Value: c.clock.Now().Sub(start).Seconds()})
	if err != nil {
		emit(collector.Metric{Name: "openmetrics.up", Value: 0})
		return err
	}
	emit(collector.Metric{Name: "openmetrics.up", Value: 1})

	now := start
	defer c.sweep(now)
	e := &emitter{emit: emit, max: c.cfg.MaxSeries}
	for i := range fams {
		c.family(&fams[i], now, e)
	}
	if e.dropped > 0 {
		return fmt.Errorf("max_series %d reached: %d metrics dropped from this scrape", c.cfg.MaxSeries, e.dropped)
	}
	return nil
}

// emitter counts what one scrape emits and drops past the cap.
type emitter struct {
	emit    collector.Emit
	max, n  int
	dropped int
}

func (e *emitter) add(m collector.Metric) {
	if e.n >= e.max {
		e.dropped++
		return
	}
	e.n++
	e.emit(m)
}

func (c *Check) scrape(ctx context.Context) ([]om.Family, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", c.url, err)
	}
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0,text/plain;version=0.0.4;q=0.5,*/*;q=0.1")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: status %s", c.url, resp.Status)
	}
	fams, err := om.Parse(resp.Body, om.Options{
		Format: om.FormatFromContentType(resp.Header.Get("Content-Type")),
		Limits: om.Limits{MaxBytes: c.cfg.MaxBody},
	})
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", c.url, err)
	}
	return fams, nil
}

// name applies the rules to a base name: allow, deny, rename, namespace.
// ok is false for a metric not to send.
func (c *Check) name(base string) (string, bool) {
	if len(c.allow) > 0 {
		kept := false
		for _, rx := range c.allow {
			if rx.MatchString(base) {
				kept = true
				break
			}
		}
		if !kept {
			return "", false
		}
	}
	for _, rx := range c.deny {
		if rx.MatchString(base) {
			return "", false
		}
	}
	if r, ok := c.cfg.Rename[base]; ok {
		base = r
	}
	if c.cfg.Namespace != "" {
		base = c.cfg.Namespace + "." + base
	}
	return base, true
}

// tags turns labels into tags, leaving out skip and the excluded ones.
func (c *Check) tags(labels []om.Label, skip string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name == skip || c.excludeLabels[l.Name] {
			continue
		}
		out = append(out, l.Name+":"+l.Value)
	}
	return out
}

// seriesKey identifies a series between scrapes: its name and label set in
// a canonical order.
func seriesKey(name string, labels []om.Label, skip string) string {
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != skip {
			parts = append(parts, l.Name+"\xff"+l.Value)
		}
	}
	sort.Strings(parts)
	return name + "\x00" + strings.Join(parts, "\xfe")
}

func (c *Check) family(f *om.Family, now time.Time, e *emitter) {
	switch f.Type {
	case om.TypeCounter:
		for _, s := range f.Samples {
			if strings.HasSuffix(s.Name, "_created") && s.Name != f.Name {
				continue // a start time, not a count
			}
			name, ok := c.name(s.Name)
			if !ok {
				continue
			}
			if r, ok := c.rates.Observe(seriesKey(s.Name, s.Labels, ""), s.Value, now); ok {
				e.add(collector.Metric{Name: name, Kind: collector.Rate, Value: r, Tags: c.tags(s.Labels, "")})
			}
		}
	case om.TypeHistogram:
		c.histogram(f, now, e)
	case om.TypeSummary:
		c.summary(f, now, e)
	case om.TypeGaugeHistogram:
		name, ok := c.name(f.Name)
		if !ok {
			return
		}
		for _, s := range f.Samples {
			m := collector.Metric{Value: s.Value, Tags: c.tags(s.Labels, "le")}
			switch s.Name {
			case f.Name + "_gbucket":
				le, _ := s.Label("le")
				m.Name, m.Tags = name+".bucket", append(m.Tags, "upper_bound:"+le)
			case f.Name + "_gsum":
				m.Name = name + ".gsum"
			case f.Name + "_gcount":
				m.Name = name + ".gcount"
			default:
				continue
			}
			e.add(m)
		}
	default: // gauge, unknown, info, stateset
		for _, s := range f.Samples {
			if name, ok := c.name(s.Name); ok {
				e.add(collector.Metric{Name: name, Value: s.Value, Tags: c.tags(s.Labels, "")})
			}
		}
	}
}

func (c *Check) histogram(f *om.Family, now time.Time, e *emitter) {
	name, ok := c.name(f.Name)
	if !ok {
		return
	}
	hs, err := f.Histograms()
	if err != nil {
		c.log.Debug("skipping a malformed histogram", "family", f.Name, "error", err)
		return
	}
	for _, h := range hs {
		tags := c.tags(h.Labels, "")
		key := seriesKey(f.Name, h.Labels, "")
		prev, seen := c.buckets[key]
		c.buckets[key] = bucketState{buckets: h.Buckets, at: now}
		if seen && c.cfg.HistogramBucketsAsDistributions {
			deltas, _ := om.BucketDeltas(prev.buckets, h.Buckets)
			sk := sketch.NewDefault()
			if err := om.ToSketch(deltas, sk); err != nil {
				c.log.Debug("skipping a histogram that does not fit a sketch", "family", f.Name, "error", err)
			} else if sk.Count() > 0 {
				e.add(collector.Metric{Name: name, Kind: collector.Distribution, Sketch: sk, Tags: tags})
			}
		} else if seen {
			deltas, _ := om.BucketDeltas(prev.buckets, h.Buckets)
			for _, b := range deltas {
				e.add(collector.Metric{
					Name: name + ".bucket", Kind: collector.Count, Value: b.Count,
					Tags: append(append([]string(nil), tags...), "upper_bound:"+formatBound(b.UpperBound)),
				})
			}
		}
		c.sumCount(name, key, h.HasSum, h.Sum, h.HasCount, h.Count, tags, now, e)
	}
}

func (c *Check) summary(f *om.Family, now time.Time, e *emitter) {
	name, ok := c.name(f.Name)
	if !ok {
		return
	}
	type sc struct {
		labels           []om.Label
		sum, count       float64
		hasSum, hasCount bool
	}
	var order []string
	byKey := map[string]*sc{}
	get := func(labels []om.Label) (*sc, string) {
		k := seriesKey(f.Name, labels, "")
		if byKey[k] == nil {
			byKey[k] = &sc{labels: labels}
			order = append(order, k)
		}
		return byKey[k], k
	}
	for _, s := range f.Samples {
		switch s.Name {
		case f.Name:
			if q, ok := s.Label("quantile"); ok && !math.IsNaN(s.Value) {
				e.add(collector.Metric{Name: name, Value: s.Value, Tags: append(c.tags(s.Labels, "quantile"), "quantile:"+q)})
			}
		case f.Name + "_sum":
			x, _ := get(s.Labels)
			x.sum, x.hasSum = s.Value, true
		case f.Name + "_count":
			x, _ := get(s.Labels)
			x.count, x.hasCount = s.Value, true
		}
	}
	for _, k := range order {
		x := byKey[k]
		c.sumCount(name, k, x.hasSum, x.sum, x.hasCount, x.count, c.tags(x.labels, ""), now, e)
	}
}

// sumCount emits a histogram's or summary's .sum and .count as counts of
// the interval.
func (c *Check) sumCount(name, key string, hasSum bool, sum float64, hasCount bool, count float64, tags []string, now time.Time, e *emitter) {
	if hasSum {
		if d, ok := c.counts.observe(key+"\x00sum", sum, now); ok {
			e.add(collector.Metric{Name: name + ".sum", Kind: collector.Count, Value: d, Tags: tags})
		}
	}
	if hasCount {
		if d, ok := c.counts.observe(key+"\x00count", count, now); ok {
			e.add(collector.Metric{Name: name + ".count", Kind: collector.Count, Value: d, Tags: tags})
		}
	}
}

// formatBound writes a bucket bound the way the page does: +Inf, or the
// shortest decimal.
func formatBound(ub float64) string {
	if math.IsInf(ub, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(ub, 'g', -1, 64)
}

// sweep forgets series unseen for a while: the rates' own rule
// (collector.Rates.Sweep), applied to the counts and buckets too.
func (c *Check) sweep(now time.Time) {
	keep := collector.ForgetAfter
	if !c.swept.IsZero() {
		keep = max(keep, 3*now.Sub(c.swept))
	}
	c.swept = now
	cutoff := now.Add(-keep)
	c.rates.Sweep(now)
	c.counts.prune(cutoff)
	for k, b := range c.buckets {
		if b.at.Before(cutoff) {
			delete(c.buckets, k)
		}
	}
}

// deltas turns cumulative counts into the increase since the previous
// reading. Unlike collector.Rates it keeps the count, not a per-second
// rate: a histogram's _count is sent as "observations this interval". A
// count that went down means the target restarted, and the new value is
// then what happened since — the same rule as the buckets'.
type deltas struct {
	last map[string]reading
}

type reading struct {
	v  float64
	at time.Time
}

func newDeltas() *deltas { return &deltas{last: map[string]reading{}} }

func (d *deltas) observe(key string, v float64, at time.Time) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		delete(d.last, key)
		return 0, false
	}
	prev, ok := d.last[key]
	d.last[key] = reading{v: v, at: at}
	if !ok {
		return 0, false
	}
	if v < prev.v {
		return v, true
	}
	return v - prev.v, true
}

func (d *deltas) prune(cutoff time.Time) {
	for k, r := range d.last {
		if r.at.Before(cutoff) {
			delete(d.last, k)
		}
	}
}
