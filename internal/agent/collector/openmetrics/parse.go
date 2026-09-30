package openmetrics

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Type is a metric family's type, from its `# TYPE` line. A family with no
// TYPE line is [TypeUnknown] — what Prometheus text calls "untyped" and
// OpenMetrics calls "unknown"; both spellings parse to it, because the
// distinction is only which format said it and a consumer treats both alike
// (as a gauge).
type Type string

// Family types. Info and stateset are OpenMetrics' two kinds of gauge that
// carry their meaning in labels: an info family's samples are always 1 and
// exist to attach labels (`build_info{version="1.2"} 1`); a stateset's samples
// are 0 or 1, one per state. A consumer can treat both as gauges.
const (
	TypeCounter        Type = "counter"
	TypeGauge          Type = "gauge"
	TypeHistogram      Type = "histogram"
	TypeGaugeHistogram Type = "gaugehistogram"
	TypeSummary        Type = "summary"
	TypeInfo           Type = "info"
	TypeStateset       Type = "stateset"
	TypeUnknown        Type = "unknown"
)

// Format is which of the two text formats a body is in. They differ in
// three ways a parser has to care about: OpenMetrics ends with `# EOF` (so a
// truncated body is detectable), writes timestamps in seconds rather than
// milliseconds, and names counter samples `<family>_total`.
type Format int

// Formats. [FormatAuto] decides from the body itself: a `# EOF` line means
// OpenMetrics, its absence Prometheus text. That is sound in one direction
// only — an OpenMetrics body cut off before its EOF looks like Prometheus
// text — which is why a caller that knows the Content-Type should say so.
const (
	FormatAuto Format = iota
	FormatPrometheus
	FormatOpenMetrics
)

// FormatFromContentType maps a scrape response's Content-Type to a Format:
// `application/openmetrics-text` is OpenMetrics; anything else (normally
// `text/plain; version=0.0.4`) is left to the body to decide.
func FormatFromContentType(ct string) Format {
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(ct)), "application/openmetrics-text") {
		return FormatOpenMetrics
	}
	return FormatAuto
}

// Label is one name="value" pair, unescaped.
type Label struct {
	Name, Value string
}

// Sample is one line of a family: a full sample name (with any `_bucket`,
// `_sum`, `_count`, `_total`, `_created` suffix — the family's name is on the
// [Family]), its labels in the order written, and its value.
//
// Values keep NaN and ±Inf: `+Inf` is a legitimate bucket count bound and a
// NaN gauge is what some exporters write for "no data". Dropping them is the
// consumer's decision, not the parser's.
type Sample struct {
	Name   string
	Labels []Label
	Value  float64
	// TimestampMs is the sample's explicit timestamp in Unix milliseconds,
	// normalized from either format; meaningful only when HasTimestamp.
	TimestampMs  int64
	HasTimestamp bool
}

// Label returns the value of the named label, and whether it is present.
func (s *Sample) Label(name string) (string, bool) {
	for _, l := range s.Labels {
		if l.Name == name {
			return l.Value, true
		}
	}
	return "", false
}

// Family is a metric family: the `# TYPE`/`# HELP`/`# UNIT` metadata and the
// samples that belong to it, in the order they appeared.
type Family struct {
	Name    string
	Type    Type
	Help    string
	Unit    string
	Samples []Sample
}

// Limits bound what one Parse may consume. A scrape target is someone else's
// process: a bug there (a label with a request id in it, an unbounded loop
// writing lines) must cost this agent an error, not its memory. Zero fields
// take the [DefaultLimits] value.
type Limits struct {
	MaxBytes         int64 // the whole body
	MaxSamples       int   // sample lines, over all families
	MaxLabels        int   // labels on one sample
	MaxNameLen       int   // a metric or label name
	MaxLabelValueLen int   // one label value, after unescaping
}

// DefaultLimits are generous for any real exporter — node_exporter on a big
// host writes a few thousand samples and under 1 MiB — and small enough that
// the worst case is a bounded allocation.
var DefaultLimits = Limits{
	MaxBytes:         16 << 20,
	MaxSamples:       200_000,
	MaxLabels:        64,
	MaxNameLen:       1024,
	MaxLabelValueLen: 4096,
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.MaxBytes > 0 {
		d.MaxBytes = l.MaxBytes
	}
	if l.MaxSamples > 0 {
		d.MaxSamples = l.MaxSamples
	}
	if l.MaxLabels > 0 {
		d.MaxLabels = l.MaxLabels
	}
	if l.MaxNameLen > 0 {
		d.MaxNameLen = l.MaxNameLen
	}
	if l.MaxLabelValueLen > 0 {
		d.MaxLabelValueLen = l.MaxLabelValueLen
	}
	return d
}

// Options configure Parse. The zero value is auto-detected format and
// [DefaultLimits].
type Options struct {
	Format Format
	Limits Limits
}

// ParseError locates a syntax error or a limit hit. Line and Col are 1-based;
// Col counts bytes.
type ParseError struct {
	Line, Col int
	Msg       string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("openmetrics: line %d col %d: %s", e.Line, e.Col, e.Msg)
}

// ErrTooLarge is wrapped by the error Parse returns when the body exceeds
// Limits.MaxBytes, so a caller can count oversized targets separately from
// malformed ones.
var ErrTooLarge = errors.New("openmetrics: body exceeds the size limit")

// Parse reads one scrape body and returns its families in order of first
// appearance.
//
// It is all or nothing: any syntax error or limit hit returns an error and no
// families. A partial result would be worse than none, because the consumer
// turns counters into rates by differencing scrapes; half a page followed by a
// whole one would read as every missing counter restarting.
func Parse(r io.Reader, opts Options) ([]Family, error) {
	lim := opts.Limits.withDefaults()
	body, err := io.ReadAll(io.LimitReader(r, lim.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("openmetrics: reading body: %w", err)
	}
	if int64(len(body)) > lim.MaxBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, lim.MaxBytes)
	}
	p := &parser{lim: lim, byName: map[string]*family{}, intern: map[string]string{}}
	return p.parse(string(body), opts.Format)
}

// family is a Family under construction: typeSet records whether a TYPE
// line has been seen, so a second one is an error but the first may still
// arrive after a HELP.
type family struct {
	Family
	typeSet bool
	helpSet bool
	hasData bool
}

// pendingTS remembers where a timestamp was written, so it can be checked
// and converted once the body's format is known — which, in auto mode, is
// only at the end.
type pendingTS struct {
	fam       *family
	sample    int
	raw       float64
	line, col int
}

type parser struct {
	lim     Limits
	fams    []*family
	byName  map[string]*family
	intern  map[string]string
	samples int
	ts      []pendingTS
}

func (p *parser) parse(body string, format Format) ([]Family, error) {
	sawEOF := false
	lineNo := 0
	for len(body) > 0 {
		lineNo++
		var line string
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			line, body = body[:i], body[i+1:]
		} else {
			line, body = body, ""
		}
		line = strings.TrimSuffix(line, "\r")
		if sawEOF {
			return nil, &ParseError{lineNo, 1, "content after # EOF"}
		}
		switch {
		case strings.TrimSpace(line) == "":
			if format == FormatOpenMetrics {
				return nil, &ParseError{lineNo, 1, "blank line (not allowed in OpenMetrics)"}
			}
		case line[0] == '#':
			eof, err := p.comment(line, lineNo)
			if err != nil {
				return nil, err
			}
			sawEOF = eof
		default:
			if err := p.sample(line, lineNo); err != nil {
				return nil, err
			}
		}
	}
	if format == FormatAuto {
		format = FormatPrometheus
		if sawEOF {
			format = FormatOpenMetrics
		}
	}
	if format == FormatOpenMetrics && !sawEOF {
		return nil, &ParseError{lineNo + 1, 1, "missing # EOF (body truncated?)"}
	}
	if err := p.resolveTimestamps(format); err != nil {
		return nil, err
	}
	out := make([]Family, len(p.fams))
	for i, f := range p.fams {
		out[i] = f.Family
		if out[i].Type == "" {
			out[i].Type = TypeUnknown
		}
	}
	return out, nil
}

// comment handles a line starting with '#'. Only HELP, TYPE, UNIT and EOF
// mean anything; any other comment is ignored, as both formats allow.
func (p *parser) comment(line string, lineNo int) (eof bool, err error) {
	rest := strings.TrimLeft(line[1:], " \t")
	if rest == "EOF" {
		return true, nil
	}
	kw, rest, _ := strings.Cut(rest, " ")
	if kw != "HELP" && kw != "TYPE" && kw != "UNIT" {
		return false, nil
	}
	col := len(line) - len(rest) + 1
	rest = strings.TrimLeft(rest, " \t")
	name, text, _ := strings.Cut(rest, " ")
	if len(name) == 0 {
		return false, &ParseError{lineNo, col, kw + " without a metric name"}
	}
	if !validMetricName(name) {
		return false, &ParseError{lineNo, col, fmt.Sprintf("invalid metric name %q", name)}
	}
	if len(name) > p.lim.MaxNameLen {
		return false, &ParseError{lineNo, col, fmt.Sprintf("metric name longer than %d bytes", p.lim.MaxNameLen)}
	}
	f := p.byName[name]
	if f == nil {
		f = p.newFamily(name)
	}
	switch kw {
	case "HELP":
		if f.helpSet {
			return false, &ParseError{lineNo, 1, fmt.Sprintf("second HELP line for %q", name)}
		}
		f.Help, f.helpSet = unescapeHelp(text), true
	case "UNIT":
		f.Unit = strings.TrimSpace(text)
	case "TYPE":
		if f.typeSet {
			return false, &ParseError{lineNo, 1, fmt.Sprintf("second TYPE line for %q", name)}
		}
		if f.hasData {
			return false, &ParseError{lineNo, 1, fmt.Sprintf("TYPE line for %q after its samples", name)}
		}
		t, ok := parseType(strings.TrimSpace(text))
		if !ok {
			return false, &ParseError{lineNo, col + len(name) + 1, fmt.Sprintf("unknown type %q", strings.TrimSpace(text))}
		}
		f.Type, f.typeSet = t, true
	}
	return false, nil
}

func parseType(s string) (Type, bool) {
	switch s {
	case "untyped", "unknown":
		return TypeUnknown, true
	case "counter", "gauge", "histogram", "gaugehistogram", "summary", "info", "stateset":
		return Type(s), true
	}
	return "", false
}

func (p *parser) newFamily(name string) *family {
	f := &family{Family: Family{Name: p.str(name)}}
	p.fams = append(p.fams, f)
	p.byName[f.Name] = f
	return f
}

// suffixes lists the sample-name suffixes each family type owns. A sample
// named `foo_bucket` belongs to family `foo` only if `foo` is a histogram;
// otherwise it is a family of its own. That matters for exporters that write
// `# TYPE foo_count gauge` — a gauge that happens to end in _count.
var suffixes = []struct {
	t   Type
	sfx []string
}{
	{TypeCounter, []string{"_total", "_created"}},
	{TypeSummary, []string{"_sum", "_count", "_created"}},
	{TypeHistogram, []string{"_bucket", "_sum", "_count", "_created"}},
	{TypeGaugeHistogram, []string{"_gbucket", "_gsum", "_gcount"}},
	{TypeInfo, []string{"_info"}},
}

// familyFor finds the family a sample named name belongs to, creating an
// untyped one if nothing declared it.
func (p *parser) familyFor(name string) *family {
	if f := p.byName[name]; f != nil {
		return f
	}
	for _, e := range suffixes {
		for _, s := range e.sfx {
			base, ok := strings.CutSuffix(name, s)
			if !ok {
				continue
			}
			if f := p.byName[base]; f != nil && f.Type == e.t {
				return f
			}
		}
	}
	return p.newFamily(name)
}

// sample parses `name[{labels}] value [timestamp] [# exemplar]`.
func (p *parser) sample(line string, lineNo int) error {
	if p.samples >= p.lim.MaxSamples {
		return &ParseError{lineNo, 1, fmt.Sprintf("more than %d samples", p.lim.MaxSamples)}
	}
	sc := scanner{s: line, line: lineNo}
	name := sc.name(true)
	if name == "" {
		return sc.errf("want a metric name")
	}
	if len(name) > p.lim.MaxNameLen {
		return sc.errAt(1, fmt.Sprintf("metric name longer than %d bytes", p.lim.MaxNameLen))
	}
	var labels []Label
	if sc.peek() == '{' {
		var err error
		if labels, err = p.labels(&sc); err != nil {
			return err
		}
	}
	if !sc.spaces() {
		return sc.errf("want a space before the value")
	}
	valCol := sc.pos + 1
	tok := sc.token()
	if tok == "" {
		return sc.errf("want a value")
	}
	v, err := parseValue(tok)
	if err != nil {
		return sc.errAt(valCol, fmt.Sprintf("invalid value %q", tok))
	}
	s := Sample{Name: p.str(name), Labels: labels, Value: v}
	var ts *pendingTS
	if sc.spaces() && sc.peek() != '#' && !sc.done() {
		tsCol := sc.pos + 1
		tok := sc.token()
		raw, err := strconv.ParseFloat(tok, 64)
		if err != nil || math.IsNaN(raw) || math.IsInf(raw, 0) {
			return sc.errAt(tsCol, fmt.Sprintf("invalid timestamp %q", tok))
		}
		ts = &pendingTS{raw: raw, line: lineNo, col: tsCol}
		sc.spaces()
	}
	// An exemplar (`# {trace_id="…"} 0.3`) is OpenMetrics' pointer from a
	// bucket to one traced request. Nothing here consumes it, so it is
	// skipped rather than validated: a malformed exemplar should not cost
	// the counters on the same line.
	if !sc.done() && sc.peek() != '#' {
		return sc.errf(fmt.Sprintf("unexpected %q after the value", sc.rest()))
	}
	f := p.familyFor(name)
	f.hasData = true
	f.Samples = append(f.Samples, s)
	p.samples++
	if ts != nil {
		ts.fam, ts.sample = f, len(f.Samples)-1
		p.ts = append(p.ts, *ts)
	}
	return nil
}

// labels parses `{name="value",…}`, allowing the trailing comma both
// formats' reference parsers accept.
func (p *parser) labels(sc *scanner) ([]Label, error) {
	sc.pos++ // '{'
	var out []Label
	for {
		sc.spaces()
		if sc.peek() == '}' {
			sc.pos++
			return out, nil
		}
		if len(out) >= p.lim.MaxLabels {
			return nil, sc.errf(fmt.Sprintf("more than %d labels", p.lim.MaxLabels))
		}
		nameCol := sc.pos + 1
		name := sc.name(false)
		if name == "" {
			return nil, sc.errf("want a label name")
		}
		if len(name) > p.lim.MaxNameLen {
			return nil, sc.errAt(nameCol, fmt.Sprintf("label name longer than %d bytes", p.lim.MaxNameLen))
		}
		for _, l := range out {
			if l.Name == name {
				return nil, sc.errAt(nameCol, fmt.Sprintf("duplicate label %q", name))
			}
		}
		sc.spaces()
		if sc.peek() != '=' {
			return nil, sc.errf("want '=' after a label name")
		}
		sc.pos++
		sc.spaces()
		val, err := sc.quoted(p.lim.MaxLabelValueLen)
		if err != nil {
			return nil, err
		}
		out = append(out, Label{Name: p.str(name), Value: val})
		sc.spaces()
		switch sc.peek() {
		case ',':
			sc.pos++
		case '}':
		default:
			return nil, sc.errf("want ',' or '}' after a label value")
		}
	}
}

// resolveTimestamps converts every explicit timestamp to milliseconds now
// that the format is known: Prometheus text writes integer milliseconds,
// OpenMetrics (possibly fractional) seconds.
func (p *parser) resolveTimestamps(format Format) error {
	for _, t := range p.ts {
		var ms float64
		if format == FormatOpenMetrics {
			ms = t.raw * 1000
		} else {
			if t.raw != math.Trunc(t.raw) {
				return &ParseError{t.line, t.col, "timestamp must be integer milliseconds in Prometheus text"}
			}
			ms = t.raw
		}
		if ms > math.MaxInt64/2 || ms < math.MinInt64/2 {
			return &ParseError{t.line, t.col, "timestamp out of range"}
		}
		s := &t.fam.Samples[t.sample]
		s.TimestampMs, s.HasTimestamp = int64(math.Round(ms)), true
	}
	return nil
}

// str interns a name. Names repeat on every line of a family and every
// scrape of a target; one copy per distinct name keeps a large page from
// costing a string per sample for each.
func (p *parser) str(s string) string {
	if v, ok := p.intern[s]; ok {
		return v
	}
	s = strings.Clone(s)
	p.intern[s] = s
	return s
}

// parseValue accepts what both formats write: decimal floats, and NaN,
// +Inf, -Inf (Go's ParseFloat also takes "Inf" and "inf", as Prometheus'
// own parser does).
func parseValue(s string) (float64, error) {
	if strings.ContainsAny(s, "_xXpP") {
		return 0, errors.New("not a decimal float")
	}
	return strconv.ParseFloat(s, 64)
}

func validMetricName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isNameStart(c) && c != ':' && (i == 0 || !isDigit(c)) {
			return false
		}
	}
	return s != ""
}

func isNameStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isDigit(c byte) bool     { return c >= '0' && c <= '9' }

// unescapeHelp undoes HELP escaping (\\, \n, and OpenMetrics' \"). An unknown
// escape is kept literally: help text is documentation, not identity, and
// rejecting a page over a stray backslash in it would cost every metric on it.
func unescapeHelp(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case '\\', '"':
			b.WriteByte(s[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// scanner walks one line. pos is a byte offset; errors report pos+1.
type scanner struct {
	s    string
	pos  int
	line int
}

func (sc *scanner) done() bool { return sc.pos >= len(sc.s) }
func (sc *scanner) rest() string {
	return sc.s[sc.pos:]
}

func (sc *scanner) peek() byte {
	if sc.done() {
		return 0
	}
	return sc.s[sc.pos]
}

// spaces skips spaces and tabs and reports whether it skipped any.
func (sc *scanner) spaces() bool {
	start := sc.pos
	for !sc.done() && (sc.s[sc.pos] == ' ' || sc.s[sc.pos] == '\t') {
		sc.pos++
	}
	return sc.pos > start
}

// name reads a metric name (colons allowed) or a label name.
func (sc *scanner) name(metric bool) string {
	start := sc.pos
	for !sc.done() {
		c := sc.s[sc.pos]
		if isNameStart(c) || (metric && c == ':') || (sc.pos > start && isDigit(c)) {
			sc.pos++
			continue
		}
		break
	}
	return sc.s[start:sc.pos]
}

// token reads up to the next space or tab.
func (sc *scanner) token() string {
	start := sc.pos
	for !sc.done() && sc.s[sc.pos] != ' ' && sc.s[sc.pos] != '\t' {
		sc.pos++
	}
	return sc.s[start:sc.pos]
}

// quoted reads a double-quoted label value, unescaping \\, \" and \n. Any
// other escape is an error: unlike HELP, a label value is part of a series'
// identity, and guessing at it would put data under a name nobody wrote.
func (sc *scanner) quoted(max int) (string, error) {
	if sc.peek() != '"' {
		return "", sc.errf("want '\"' to open a label value")
	}
	sc.pos++
	start := sc.pos
	var b *strings.Builder
	for !sc.done() {
		c := sc.s[sc.pos]
		switch c {
		case '"':
			// Checked before copying, so an oversized value costs nothing.
			n := sc.pos - start
			if b != nil {
				n = b.Len()
			}
			if n > max {
				return "", sc.errAt(start, fmt.Sprintf("label value longer than %d bytes", max))
			}
			var v string
			if b != nil {
				v = b.String()
			} else {
				v = strings.Clone(sc.s[start:sc.pos])
			}
			sc.pos++
			return v, nil
		case '\\':
			if b == nil {
				b = &strings.Builder{}
				b.WriteString(sc.s[start:sc.pos])
			}
			if sc.pos+1 >= len(sc.s) {
				return "", sc.errf("unterminated escape in a label value")
			}
			switch e := sc.s[sc.pos+1]; e {
			case '\\', '"':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			default:
				return "", sc.errf(fmt.Sprintf(`invalid escape \%c in a label value`, e))
			}
			sc.pos += 2
		default:
			if b != nil {
				b.WriteByte(c)
			}
			sc.pos++
		}
		if b != nil && b.Len() > max {
			return "", sc.errAt(start, fmt.Sprintf("label value longer than %d bytes", max))
		}
	}
	return "", sc.errAt(start, "unterminated label value")
}

func (sc *scanner) errf(msg string) error { return sc.errAt(sc.pos+1, msg) }
func (sc *scanner) errAt(col int, msg string) error {
	return &ParseError{Line: sc.line, Col: col, Msg: msg}
}
