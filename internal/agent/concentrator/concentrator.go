package concentrator

import (
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tuvo1106/ozymandias/internal/agent/aggregator"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Other is the value an over-cap resource, service or span name folds into.
const Other = "_other_"

// Defaults for the cardinality caps.
const (
	DefaultMaxResources = 500
	DefaultMaxServices  = 200
	DefaultMaxNames     = 100
	// maxResourceTag leaves room for the "resource:" key inside the 200-byte tag limit.
	maxResourceTag = 150
	// ResetEvery clears the cap bookkeeping so a burst of one-off resources does
	// not pin a service to `_other_` until the agent restarts.
	ResetEvery = time.Hour
)

// Adder is the part of the aggregator the concentrator needs.
type Adder interface {
	Add(s aggregator.Sample, now time.Time)
}

// Options configure a Concentrator.
type Options struct {
	Sink Adder
	// Env is the default env tag for spans that carry none.
	Env                                 string
	MaxResources, MaxServices, MaxNames int
	Registry                            *selfmetrics.Registry
}

// Concentrator turns spans into RED metrics. It is safe for concurrent use.
type Concentrator struct {
	opts Options

	mu       sync.Mutex
	epoch    int64 // unix hour of the current cap window
	services map[string]map[string]struct{}
	names    map[string]struct{}
	folded   *selfmetrics.Counter
	observed *selfmetrics.Counter
}

// New returns a Concentrator.
func New(opts Options) *Concentrator {
	if opts.MaxResources <= 0 {
		opts.MaxResources = DefaultMaxResources
	}
	if opts.MaxServices <= 0 {
		opts.MaxServices = DefaultMaxServices
	}
	if opts.MaxNames <= 0 {
		opts.MaxNames = DefaultMaxNames
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	return &Concentrator{
		opts: opts, services: map[string]map[string]struct{}{}, names: map[string]struct{}{},
		folded:   opts.Registry.Counter("ozy.agent.traces.stats_folded"),
		observed: opts.Registry.Counter("ozy.agent.traces.stats_spans"),
	}
}

// Observe counts the stat-bearing spans of spans. Call it on every span the
// agent receives, before the samplers run.
func (c *Concentrator) Observe(spans []wire.Span) {
	for i := range spans {
		sp := &spans[i]
		if !sp.TopLevel() && !sp.Measured() {
			continue
		}
		c.observed.Inc()
		end := time.UnixMicro(sp.End())
		service, resource, name := c.bound(sp)
		env := sp.Meta["env"]
		if env == "" {
			env = c.opts.Env
		}
		tags := []string{"service:" + tagValue(service), "resource:" + tagValue(resource)}
		if env != "" {
			tags = append(tags, "env:"+tagValue(env))
		}
		if cls := StatusClass(sp.Meta["http.status_code"]); cls != "" {
			tags = append(tags, "status_class:"+cls)
		}
		base := "trace." + name
		c.opts.Sink.Add(aggregator.Sample{Name: base + ".hits", Kind: aggregator.Counter, Value: 1, Tags: tags}, end)
		if sp.Error == 1 {
			c.opts.Sink.Add(aggregator.Sample{Name: base + ".errors", Kind: aggregator.Counter, Value: 1, Tags: tags}, end)
		}
		c.opts.Sink.Add(aggregator.Sample{Name: base + ".duration", Kind: aggregator.Distribution, Value: float64(sp.Duration) / 1e6, Tags: tags}, end)
	}
}

// bound applies the caps and returns the service, resource and name to tag with.
func (c *Concentrator) bound(sp *wire.Span) (service, resource, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h := time.Now().Unix() / int64(ResetEvery.Seconds()); h != c.epoch {
		c.epoch = h
		clear(c.services)
		clear(c.names)
	}
	service, resource, name = sp.Service, sp.Resource, sp.Name
	if _, ok := c.names[name]; !ok {
		if len(c.names) >= c.opts.MaxNames {
			name = "other"
			c.folded.Inc()
		} else {
			c.names[name] = struct{}{}
		}
	}
	res, ok := c.services[service]
	if !ok {
		if len(c.services) >= c.opts.MaxServices {
			c.folded.Inc()
			return Other, Other, name
		}
		res = map[string]struct{}{}
		c.services[service] = res
	}
	key := tagValue(resource)
	if _, ok := res[key]; !ok {
		if len(res) >= c.opts.MaxResources {
			c.folded.Inc()
			return service, Other, name
		}
		res[key] = struct{}{}
	}
	return service, resource, name
}

// StatusClass maps an HTTP status code string to "2xx" and so on, or "" when it
// is missing or not a status.
func StatusClass(code string) string {
	n, err := strconv.Atoi(code)
	if err != nil || n < 100 || n > 599 {
		return ""
	}
	return strconv.Itoa(n/100) + "xx"
}

// tagValue makes s legal in a tag: no commas or newlines (which ValidTag
// refuses), at most maxResourceTag bytes cut on a rune boundary.
func tagValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == ',' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxResourceTag {
		cut := maxResourceTag
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
