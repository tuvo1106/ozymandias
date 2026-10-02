package openmetrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
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
	// MaxSeries bounds the metrics one scrape emits, and the series whose
	// last reading the check keeps between scrapes. Default 2000.
	MaxSeries int `yaml:"max_series"`
}

// Check scrapes one target.
type Check struct {
	cfg           Config
	url           string
	shown         string // url as errors print it (collector.RedactURL): they are logged
	allow, deny   []*regexp.Regexp
	excludeLabels map[string]bool
	client        *http.Client
	clock         clock.Clock
	log           *slog.Logger

	rates   *collector.Rates
	counts  *collector.Rates // used with Delta: increases, not rates
	buckets map[string]bucketState
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
	_, err := collector.ParseCheckURL(cfg.URL)
	if err != nil {
		return nil, err
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
		cfg: cfg, url: cfg.URL, shown: collector.RedactURL(cfg.URL), excludeLabels: map[string]bool{},
		client: &http.Client{Timeout: cfg.Timeout}, clock: clk, log: log,
		rates: collector.NewRates(), counts: collector.NewRates(), buckets: map[string]bucketState{},
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
		return fmt.Errorf("max_series %d reached: %d metrics or series dropped from this scrape", c.cfg.MaxSeries, e.dropped)
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

// admit says whether a series may be tracked between scrapes: one already
// tracked always may, a new one only while the rates, buckets and counts
// together hold fewer than max_series. Capping only what is sent would
// still keep a reading of every series a target serves — 150k of them for
// a label with a request id in it — for as long as it serves them, which
// is the growth max_series is there to stop. A series refused is counted
// as dropped, like a metric past the cap.
func (c *Check) admit(tracked bool, e *emitter) bool {
	if tracked || c.rates.Len()+len(c.buckets)+c.counts.Len() < c.cfg.MaxSeries {
		return true
	}
	e.dropped++
	return false
}

func (c *Check) scrape(ctx context.Context) ([]om.Family, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", c.shown, collector.RequestError(c.shown, err))
	}
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0,text/plain;version=0.0.4;q=0.5,*/*;q=0.1")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", c.shown, collector.RequestError(c.shown, err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: status %s", c.shown, resp.Status)
	}
	fams, err := om.Parse(resp.Body, om.Options{
		Format: om.FormatFromContentType(resp.Header.Get("Content-Type")),
		Limits: om.Limits{MaxBytes: c.cfg.MaxBody},
	})
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", c.shown, collector.RequestError(c.shown, err))
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
//
// A label named host becomes exported_host. The agent lets a check's own
// host tag stand in for its (a check that measures another machine says
// so), but a scraped page's host is the page's word, and on most pages it
// is not a machine: it is an HTTP Host header or a virtual host
// (http_requests_total{host="api.example.com"}). Kept as host, it would move
// the series off the agent's machine and merge every machine scraping such
// a page under one name.
func (c *Check) tags(labels []om.Label, skip string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name == skip || c.excludeLabels[l.Name] {
			continue
		}
		name := l.Name
		if name == "host" {
			name = "exported_host"
		}
		out = append(out, name+":"+l.Value)
	}
	return out
}

// seriesKey is a sample's identity: its name and its label set, by the
// parser's one rule for "same series" (om.LabelKey).
func seriesKey(name string, labels []om.Label) string {
	return name + "\x00" + om.LabelKey(labels, "")
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
			key := seriesKey(s.Name, s.Labels)
			if !c.admit(c.rates.Has(key), e) {
				continue
			}
			if r, ok := c.rates.Observe(key, s.Value, now); ok {
				e.add(collector.Metric{Name: name, Kind: collector.Rate, Value: r, Tags: c.tags(s.Labels, "")})
			}
		}
	case om.TypeHistogram:
		c.histogram(f, now, e)
	case om.TypeSummary:
		c.summary(f, now, e)
	case om.TypeGaugeHistogram:
		c.gaugeHistogram(f, e)
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
		key := f.Name + "\x00" + h.Key // seriesKey(f.Name, h.Labels), already computed
		prev, seen := c.buckets[key]
		if !c.admit(seen, e) {
			continue
		}
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

// gaugeHistogram reports a gauge histogram's buckets, gsum and gcount as
// levels: each sample is the current state, not a running total, so there
// is nothing to difference. It goes through the same grouping as a
// histogram (Family.Histograms), so le="1.0" is upper_bound:1 here as
// there — one query matches both — and a duplicate le or a missing +Inf is
// handled the same way: the family is skipped, or the +Inf reconstructed.
func (c *Check) gaugeHistogram(f *om.Family, e *emitter) {
	name, ok := c.name(f.Name)
	if !ok {
		return
	}
	hs, err := f.Histograms()
	if err != nil {
		c.log.Debug("skipping a malformed gauge histogram", "family", f.Name, "error", err)
		return
	}
	for _, h := range hs {
		tags := c.tags(h.Labels, "")
		for _, b := range h.Buckets {
			e.add(collector.Metric{
				Name: name + ".bucket", Value: b.Count,
				Tags: append(append([]string(nil), tags...), "upper_bound:"+formatBound(b.UpperBound)),
			})
		}
		if h.HasSum {
			e.add(collector.Metric{Name: name + ".gsum", Value: h.Sum, Tags: tags})
		}
		if h.HasCount {
			e.add(collector.Metric{Name: name + ".gcount", Value: h.Count, Tags: tags})
		}
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
		k := seriesKey(f.Name, labels)
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
//
// The change since the last good scrape, whatever its age: after a missed
// scrape one point carries two intervals' observations and the missed one
// has none. That is a count's meaning (events since the last point), and
// as_rate divides each query bucket's sum by the bucket's width, so totals
// and rates over any window that spans both are exact; only a 15s view
// shows the pair as a spike beside a gap. Scaling the value to one
// interval would instead lose the missed interval's events from every sum.
//
// A restart is read from the _count, which only grows, not from the _sum:
// a histogram of negative observations (a temperature change, a clock
// offset) has a sum that legitimately falls, and taking that fall for a
// reset would send the whole lifetime sum as one interval's change. So a
// fall in the count is a restart, and both increases are then the new
// values; otherwise the sum's change is its difference, negative or not.
// A sum with no count beside it has nothing to tell a restart by, and keeps
// the counter rule: a fall is a restart.
func (c *Check) sumCount(name, key string, hasSum bool, sum float64, hasCount bool, count float64, tags []string, now time.Time, e *emitter) {
	sumKey, countKey := key+"\x00sum", key+"\x00count"
	var (
		cd         float64
		cok, reset bool
	)
	if hasCount && c.admit(c.counts.Has(countKey), e) {
		if cd, cok = c.counts.Change(countKey, count, now); cok && cd < 0 {
			cd, reset = count, true
		}
	}
	if hasSum && c.admit(c.counts.Has(sumKey), e) {
		var (
			d  float64
			ok bool
		)
		if hasCount {
			if d, ok = c.counts.Change(sumKey, sum, now); ok && reset {
				d = sum
			}
		} else if d, ok = c.counts.Change(sumKey, sum, now); ok && d < 0 {
			d = sum // no count to ask: a fall is a restart, as for a counter
		}
		if ok {
			e.add(collector.Metric{Name: name + ".sum", Kind: collector.Count, Value: d, Tags: tags})
		}
	}
	if cok {
		e.add(collector.Metric{Name: name + ".count", Kind: collector.Count, Value: cd, Tags: tags})
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

// sweep forgets series unseen for a while: the rates' rule
// (collector.Rates.Sweep), whose cutoff the buckets use too.
func (c *Check) sweep(now time.Time) {
	cutoff := c.rates.Sweep(now)
	c.counts.Sweep(now)
	for k, b := range c.buckets {
		if b.at.Before(cutoff) {
			delete(c.buckets, k)
		}
	}
}
