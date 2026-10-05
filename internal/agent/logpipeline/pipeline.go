package logpipeline

import (
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// DefaultRateLimit is the lines per second a source may emit when rate_limit is
// not set: none. A limit drops the excess for good, and the limiter is driven
// by the poll's clock, so a backlog read after an ozyd outage (many seconds of
// lines in one poll) would look like a storm and be mostly dropped although the
// app never exceeded its rate. A limit is therefore opt-in, for a source known
// to be able to flood.
const DefaultRateLimit = 0

// DefaultRailsGroupTimeout is how long a Rails request may stay open without
// its "Completed" line before it is emitted as incomplete.
const DefaultRailsGroupTimeout = 10 * time.Second

// Meta is what the tailer knows about a line besides its text.
type Meta struct {
	Service string // the stream's service; a log's own "service" field is used only if this is empty
	Source  string // the source label (winston, python, ...)
	Host    string
	Tags    []string
	// Stderr is true for a line the container wrote to stderr. It decides the
	// status only when no parser found one: uvicorn writes INFO to stderr.
	Stderr bool
	// Received is when the agent read the line; it stands in for a timestamp the
	// line does not carry.
	Received time.Time
}

// Spec configures one source's pipeline.
type Spec struct {
	// Source names the built-in pipeline: json (the default for an unknown
	// name), winston, caddy, python, rails, sidekiq, postgres, redis, plain.
	Source string `yaml:"source"`
	// Grok adds patterns tried before the built-ins.
	Grok []GrokSpec `yaml:"grok"`
	// Redact adds to, or switches off, the default redaction rules.
	Redact RedactConfig `yaml:"redact"`
	// ExcludeAtMatch drops any line matching one of these regular expressions,
	// before it is parsed (health checks, for one).
	ExcludeAtMatch []string `yaml:"exclude_at_match"`
	// RateLimit is lines per second, the excess dropped and counted. Zero (the
	// default) and negative mean unlimited. Opt-in: see DefaultRateLimit.
	RateLimit int `yaml:"rate_limit"`
	// RailsGroupTimeout applies to the rails source; zero means the default.
	RailsGroupTimeout time.Duration `yaml:"rails_group_timeout"`
}

// Options are the pipeline's dependencies.
type Options struct {
	Clock clock.Clock // default clock.Real()
	// Totals, if set, receives this pipeline's counts (shared with others).
	Totals *Totals
}

// Stats counts what happened to the lines a pipeline saw.
type Stats struct {
	Lines       int64 // lines offered
	Emitted     int64 // logs produced
	Excluded    int64 // dropped by exclude_at_match
	RateLimited int64 // dropped by the rate limit
	Unparsed    int64 // lines no parser recognized (kept as plain text)
	Redactions  int64 // strings the redactor changed
}

// Totals are the counters of one or more pipelines. Pipelines built with the
// same Totals add into it, so the agent can report one number for every file and
// container it follows, including the ones that have since gone away.
type Totals struct{ lines, emitted, excluded, rateLimited, unparsed, redactions atomic.Int64 }

// Stats returns the counters so far.
func (t *Totals) Stats() Stats {
	return Stats{
		Lines: t.lines.Load(), Emitted: t.emitted.Load(), Excluded: t.excluded.Load(),
		RateLimited: t.rateLimited.Load(), Unparsed: t.unparsed.Load(), Redactions: t.redactions.Load(),
	}
}

// Pipeline turns lines into logs: exclude, rate-limit, parse, remap, redact.
// It is safe for use by one goroutine at a time (a source's tailer); the
// counters can be read from any.
type Pipeline struct {
	source  string
	parsers []parser
	red     *Redactor
	exclude []*regexp.Regexp
	lim     *limiter
	rails   *railsGrouper
	clk     clock.Clock
	n       *Totals
}

// New builds a pipeline from spec.
func New(spec Spec, opts Options) (*Pipeline, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	ps, err := buildParsers(spec.Source, spec.Grok)
	if err != nil {
		return nil, err
	}
	if opts.Totals == nil {
		opts.Totals = &Totals{}
	}
	p := &Pipeline{source: spec.Source, parsers: ps, clk: opts.Clock, n: opts.Totals}
	if p.red, err = NewRedactor(spec.Redact, func(string) { p.n.redactions.Add(1) }); err != nil {
		return nil, err
	}
	for i, ex := range spec.ExcludeAtMatch {
		re, err := regexp.Compile(ex)
		if err != nil {
			return nil, fmt.Errorf("exclude_at_match[%d] %q: %w", i, ex, err)
		}
		p.exclude = append(p.exclude, re)
	}
	rate := spec.RateLimit
	if rate == 0 {
		rate = DefaultRateLimit
	}
	p.lim = newLimiter(rate, opts.Clock.Now())
	if spec.Source == "rails" {
		to := spec.RailsGroupTimeout
		if to <= 0 {
			to = DefaultRailsGroupTimeout
		}
		p.rails = newRailsGrouper(to)
	}
	return p, nil
}

// Stats returns the counters so far (shared with any pipeline using the same Totals).
func (p *Pipeline) Stats() Stats { return p.n.Stats() }

// Process runs one line. It returns the logs it produced: usually one, none for
// a line that was dropped or is part of a Rails request still being read, and
// several when a request completes and others had to be flushed to make room.
func (p *Pipeline) Process(line string, m Meta) []wire.Log {
	if m.Received.IsZero() {
		m.Received = p.clk.Now()
	}
	p.n.lines.Add(1)
	for _, re := range p.exclude {
		if re.MatchString(line) {
			p.n.excluded.Add(1)
			return nil
		}
	}
	if !p.lim.allow(m.Received) {
		p.n.rateLimited.Add(1)
		return nil
	}
	if p.rails != nil {
		if done, handled := p.rails.consume(line, m); handled {
			return p.finishAll(done)
		}
	}
	var pr parsed
	ok := false
	for _, ps := range p.parsers {
		if ps.parse(line, &pr) {
			ok = true
			break
		}
	}
	if !ok {
		p.n.unparsed.Add(1)
		pr.fields = map[string]any{"message": line}
	}
	return []wire.Log{p.finish(pr.fields, m, ok)}
}

// Flush emits the Rails requests that have been open too long. The tailer
// calls it on its tick.
func (p *Pipeline) Flush(now time.Time) []wire.Log {
	if p.rails == nil {
		return nil
	}
	return p.finishAll(p.rails.flush(now))
}

// FlushAll emits everything still buffered, for shutdown.
func (p *Pipeline) FlushAll() []wire.Log {
	if p.rails == nil {
		return nil
	}
	return p.finishAll(p.rails.flushAll())
}

func (p *Pipeline) finishAll(evs []groupedEvent) []wire.Log {
	out := make([]wire.Log, 0, len(evs))
	for _, e := range evs {
		out = append(out, p.finish(e.fields, e.meta, true))
	}
	return out
}

// Field names the remappers read, in priority order. Their spelling is the
// plan's (docs/plan/M4-logs.md §2): what apps actually call these.
var (
	messageKeys   = []string{"message", "msg"}
	statusKeys    = []string{"level", "levelname", "severity"}
	timestampKeys = []string{"timestamp", "time", "ts", "@timestamp"}
)

// finish applies the remappers, the reserved-field rule and redaction, and
// builds the log. fields is consumed.
func (p *Pipeline) finish(fields map[string]any, m Meta, recognized bool) wire.Log {
	// A numeric `status` is an HTTP status code, not a severity. Moving it
	// before the remappers run keeps it out of the way of the status field
	// and keeps the number, for `@status_code:>=500`.
	if v, ok := fields["status"]; ok {
		if _, isNum := numberOrNumeric(v); isNum {
			if _, taken := fields["status_code"]; !taken {
				fields["status_code"] = v
				delete(fields, "status")
			}
		}
	}

	l := wire.Log{Source: m.Source, Host: m.Host, Service: m.Service}
	if l.Source == "" {
		l.Source = p.source
	}
	l.Tags = append([]string(nil), m.Tags...)

	l.Message = takeMessage(fields)
	switch st, ok := takeStatus(fields); {
	case ok:
		l.Status = st
	case m.Stderr && !recognized:
		// No parser recognized the line, so the stream is the only hint. This is the
		// fallback and never the rule: uvicorn writes INFO lines to stderr, and
		// they carry their level.
		l.Status = wire.StatusError
	default:
		l.Status = wire.StatusInfo
	}
	if ts, ok := takeTimestamp(fields); ok {
		l.Ts = ts
	} else {
		l.Ts = m.Received.UnixMilli()
	}
	if l.Service == "" {
		if s, ok := fields["service"].(string); ok && s != "" {
			l.Service = s
			delete(fields, "service")
		}
	}
	if l.Service == "" {
		l.Service = "unknown"
	}
	takeTrace(fields, &l)

	if len(fields) > 0 {
		l.Attrs = fields
	}
	if msg, changed := p.red.String(l.Message); changed {
		l.Message = msg
	}
	if l.Attrs != nil {
		p.red.Attrs(l.Attrs)
	}
	p.n.emitted.Add(1)
	return l
}

func takeMessage(fields map[string]any) string {
	for _, k := range messageKeys {
		v, ok := fields[k]
		if !ok {
			continue
		}
		delete(fields, k)
		if s, isStr := v.(string); isStr {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

func takeStatus(fields map[string]any) (string, bool) {
	for _, k := range statusKeys {
		if st, ok := normalizeStatus(fields[k]); ok {
			delete(fields, k)
			return st, true
		}
	}
	return "", false
}

func takeTimestamp(fields map[string]any) (int64, bool) {
	for _, k := range timestampKeys {
		if ts, ok := parseTimestamp(fields[k]); ok {
			delete(fields, k)
			return ts, true
		}
	}
	return 0, false
}

// takeTrace reads trace and span ids. A decimal Datadog id (dd.trace_id) is
// read as decimal and a plain trace_id as hex: the key says which.
func takeTrace(fields map[string]any, l *wire.Log) {
	for _, c := range []struct {
		key     string
		decimal bool
	}{{"trace_id", false}, {"dd.trace_id", true}} {
		if id, ok := normalizeTraceID(fields[c.key], c.decimal); ok {
			l.TraceID = id
			delete(fields, c.key)
			break
		}
	}
	for _, c := range []struct {
		key     string
		decimal bool
	}{{"span_id", false}, {"dd.span_id", true}} {
		if id, ok := normalizeSpanID(fields[c.key], c.decimal); ok {
			l.SpanID = id
			delete(fields, c.key)
			break
		}
	}
}

// numberOrNumeric reports whether v is a number (or a numeric string a grok
// pattern left as text).
func numberOrNumeric(v any) (float64, bool) {
	switch v := v.(type) {
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case interface{ Float64() (float64, error) }: // json.Number
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}
