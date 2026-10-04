package logpipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// parsed is what a parser extracts from one line: a flat set of named
// fields, which the remappers then turn into the log's message, status,
// timestamp and trace ids. Everything left over becomes attrs.
type parsed struct {
	fields map[string]any
}

// parser reads a line into p and reports whether the line was its format. A
// parser that does not recognize the line leaves p untouched.
type parser interface {
	parse(line string, p *parsed) bool
	name() string
}

// jsonParser reads a line that is one JSON object. Numbers stay json.Number
// so an id above 2^53 survives all the way to the store.
type jsonParser struct{}

func (jsonParser) name() string { return "json" }

func (jsonParser) parse(line string, p *parsed) bool {
	t := strings.TrimSpace(line)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || dec.More() {
		return false
	}
	p.fields = m
	return true
}

// grok is a regular expression with named groups: each group becomes a field.
// A group named with a type suffix is converted (`ms_int` becomes the integer
// field `ms`, `elapsed_float` a float), because "12" the string and 12 the
// number answer different queries (`@ms:>10`), and a regex can only capture
// text. Message, if set, builds the message from the fields: `${method} ${path}`.
//
// Go's regexp is RE2, which cannot backtrack, so a pattern cannot be made to
// take exponential time on a hostile line — the property that matters for an
// agent parsing text it does not control.
type grok struct {
	id      string
	re      *regexp.Regexp
	message string
}

func (g *grok) name() string { return g.id }

func (g *grok) parse(line string, p *parsed) bool {
	m := g.re.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	fields := make(map[string]any, len(m))
	for i, name := range g.re.SubexpNames() {
		if i == 0 || name == "" || m[i] == "" {
			continue
		}
		key, val := coerce(name, m[i])
		fields[key] = val
	}
	if g.message != "" {
		fields["message"] = expandTemplate(g.message, fields)
	}
	p.fields = fields
	return true
}

// coerce applies a group name's type suffix.
func coerce(name, v string) (string, any) {
	switch {
	case strings.HasSuffix(name, "_int"):
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return strings.TrimSuffix(name, "_int"), n
		}
		return strings.TrimSuffix(name, "_int"), v
	case strings.HasSuffix(name, "_float"):
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return strings.TrimSuffix(name, "_float"), f
		}
		return strings.TrimSuffix(name, "_float"), v
	}
	return name, v
}

var templateVar = regexp.MustCompile(`\$\{(\w+)\}`)

func expandTemplate(t string, fields map[string]any) string {
	return templateVar.ReplaceAllStringFunc(t, func(s string) string {
		v, ok := fields[s[2:len(s)-1]]
		if !ok {
			return ""
		}
		return fmt.Sprint(v)
	})
}

// GrokSpec is a custom pattern from configuration, tried before the built-in
// ones for its source.
type GrokSpec struct {
	// Pattern is a regular expression with named groups. Names ending in
	// _int or _float are converted; message, level, timestamp, trace_id and
	// span_id feed the log's own fields.
	Pattern string `yaml:"pattern"`
	// Message builds the message from the fields: "${method} ${path}".
	Message string `yaml:"message"`
}

func newGrok(id, pattern, message string) (*grok, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("grok %s: %w", id, err)
	}
	return &grok{id: id, re: re, message: message}, nil
}

// The message group is (?s:.*) so that a multi-line event (a traceback the
// tailer joined to its first line) still matches its first line's pattern, with
// the whole of it as the message.
//
// The built-in patterns. Each is anchored and ordered most specific first
// within its source, because the first that matches wins: uvicorn's access
// line is also an `INFO: …` line, so the access pattern must be tried before
// the generic one.
const levels = `DEBUG|INFO|WARNING|WARN|ERROR|CRITICAL|FATAL`

var builtinGroks = map[string]struct{ pattern, message string }{
	// INFO:     127.0.0.1:58064 - "GET /api/v1/healthz HTTP/1.1" 200 OK
	"uvicorn-access": {`^(?P<level>[A-Z]+):\s+(?P<client_ip>[0-9.]+|\[[0-9a-fA-F:]+\]):(?P<client_port_int>\d+) - "(?P<method>[A-Z]+) (?P<path>\S+) HTTP/(?P<http_version>[0-9.]+)" (?P<status_code_int>\d{3})(?: (?P<reason>.*))?$`,
		"${method} ${path} ${status_code}"},
	// INFO:     Started server process [1]   /   ERROR:    Exception in ASGI application
	"uvicorn": {`^(?P<level>` + levels + `):\s+(?P<message>(?s:.*))$`, ""},
	// 2026-10-04 12:00:00,123 INFO app.module something happened
	"python-asctime": {`^(?P<timestamp>\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}[.,]\d+) (?P<level>` + levels + `) (?P<logger>[\w.\-]+) (?P<message>(?s:.*))$`, ""},
	// INFO arq.worker redis_version=7.4.11 …     (%(levelname)s %(name)s %(message)s)
	"python-logging": {`^(?P<level>` + levels + `) (?P<logger>[\w.\-]+) (?P<message>(?s:.*))$`, ""},
	// INFO:app.module:something happened        (logging.basicConfig's default)
	"python-basic": {`^(?P<level>` + levels + `):(?P<logger>[\w.\-]+):(?P<message>(?s:.*))$`, ""},
	// 21:01:00:   1.02s → cron:reap_orphans()    (arq's own clock-stamped line: no date)
	"arq-clock": {`^(?P<clock>\d{2}:\d{2}:\d{2}): +(?P<message>(?s:.*))$`, ""},
	// 2026-10-04T12:00:00.123Z pid=1 tid=gx0 class=HardWorker jid=abc INFO: start
	"sidekiq": {`^(?P<timestamp>\S+) pid=(?P<pid_int>\d+) tid=(?P<tid>\S+)(?: class=(?P<class>\S+))?(?: jid=(?P<jid>\S+))?(?: elapsed=(?P<elapsed_float>[0-9.]+))? (?P<level>[A-Z]+): (?P<message>(?s:.*))$`, ""},
	// 2026-09-24 02:40:45.365 UTC [54] LOG:  checkpoint starting: time
	"postgres": {`^(?P<timestamp>\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?(?: [A-Z]{2,5})?) \[(?P<pid_int>\d+)\](?: (?P<user>[^@\s]+)@(?P<db>\S+))? (?P<level>[A-Z]+[0-9]?):\s+(?P<message>(?s:.*))$`, ""},
	// 17:C 24 Sep 2026 02:45:46.028 * DB saved on disk
	"redis": {`^(?P<pid_int>\d+):(?P<role>[XCSM]) (?P<timestamp>\d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2}(?:\.\d+)?) (?P<level>[.\-*#]) (?P<message>(?s:.*))$`, ""},
}

// builtinParsers lists, per source, the parsers to try in order. A source
// with no entry gets JSON only.
var builtinParsers = map[string][]string{
	"json":     {"json"},
	"winston":  {"json"},
	"caddy":    {"json"},
	"python":   {"json", "uvicorn-access", "uvicorn", "python-asctime", "python-logging", "python-basic", "arq-clock"},
	"sidekiq":  {"json", "sidekiq"},
	"rails":    {"json"}, // plus the request grouper (rails.go)
	"postgres": {"postgres"},
	"redis":    {"redis"},
	"plain":    {},
}

func buildParsers(source string, custom []GrokSpec) ([]parser, error) {
	var out []parser
	for i, c := range custom {
		g, err := newGrok(fmt.Sprintf("custom-%d", i), c.Pattern, c.Message)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	names, ok := builtinParsers[source]
	if !ok {
		names = builtinParsers["json"]
	}
	for _, n := range names {
		if n == "json" {
			out = append(out, jsonParser{})
			continue
		}
		b := builtinGroks[n]
		g, err := newGrok(n, b.pattern, b.message)
		if err != nil {
			return nil, err // a built-in that does not compile is a bug, found by the tests
		}
		out = append(out, g)
	}
	return out, nil
}
