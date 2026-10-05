package logpipeline

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rails writes one request as several lines, each prefixed with the request
// id when `config.log_tags = [:request_id]` is set:
//
//	[8f3a…] Started GET "/orders?page=2" for 127.0.0.1 at 2026-10-04 12:00:00 +0000
//	[8f3a…] Processing by OrdersController#index as JSON
//	[8f3a…]   Parameters: {"page"=>"2"}
//	[8f3a…]   Order Load (0.5ms)  SELECT …
//	[8f3a…] Completed 200 OK in 12ms (Views: 1.2ms | ActiveRecord: 3.4ms | Allocations: 1234)
//
// and a busy server interleaves the lines of concurrent requests. Reading one
// request as one event is what makes `@controller:OrdersController
// @duration:>200` a query: it cannot be answered from five separate lines.
// The usual multiline trick (a line that does not start an event belongs to
// the previous one) fails here, because the previous line is another
// request's. The lines have to be correlated by the id, which is what
// railsGrouper does.
var (
	railsPrefix     = regexp.MustCompile(`^\[(?P<id>[0-9A-Za-z][0-9A-Za-z-]{7,63})\] ?(?P<rest>.*)$`)
	railsStarted    = regexp.MustCompile(`^Started (?P<method>[A-Z]+) "(?P<path>[^"]*)" for (?P<remote_ip>\S+) at (?P<time>.+)$`)
	railsProcessing = regexp.MustCompile(`^Processing by (?P<controller>[\w:]+)#(?P<action>\w+) as (?P<format>\S+)$`)
	railsParams     = regexp.MustCompile(`^\s*Parameters: (?P<params>.*)$`)
	railsCompleted  = regexp.MustCompile(`^Completed (?P<status>\d{3})(?: (?P<reason>[A-Za-z ]+?))? in (?P<ms>[0-9.]+)ms(?: \((?P<timings>.*)\))?$`)
	railsException  = regexp.MustCompile(`^(?P<class>[A-Z][\w:]*(?:Error|Exception|NotFound|Invalid))(?: \((?P<message>.*)\))?:?\s*$`)
	railsTiming     = regexp.MustCompile(`([A-Za-z]+): ([0-9.]+)(ms)?`)
)

const (
	// maxRailsGroups bounds how many requests may be in flight at once; the
	// oldest is flushed as incomplete when a new one would exceed it.
	maxRailsGroups = 1000
	// maxRailsLines bounds one request's buffered lines.
	maxRailsLines = 200
)

type railsGroup struct {
	id     string
	meta   Meta
	first  time.Time // when its first line arrived
	fields map[string]any
	lines  int
	done   bool
}

type railsGrouper struct {
	timeout time.Duration
	groups  map[string]*railsGroup
}

func newRailsGrouper(timeout time.Duration) *railsGrouper {
	return &railsGrouper{timeout: timeout, groups: map[string]*railsGroup{}}
}

// consume offers a line to the grouper. handled is false if the line is not a
// request-id-prefixed line, and the caller should parse it as an ordinary
// line. When handled, done holds the finished requests (zero or one, plus
// any flushed to make room), as field sets ready for the remappers.
func (g *railsGrouper) consume(line string, m Meta) (done []groupedEvent, handled bool) {
	pm := railsPrefix.FindStringSubmatch(line)
	if pm == nil {
		return nil, false
	}
	id, rest := pm[1], pm[2]
	grp := g.groups[id]
	if grp == nil {
		if len(g.groups) >= maxRailsGroups {
			done = append(done, g.evictOldest()...)
		}
		grp = &railsGroup{id: id, meta: m, first: m.Received, fields: map[string]any{"request_id": id}}
		g.groups[id] = grp
	}
	grp.lines++
	// Past the cap only the terminal line is read: a chatty request still ends.
	if grp.lines <= maxRailsLines || railsCompleted.MatchString(rest) {
		grp.absorb(rest)
	}
	if grp.done {
		delete(g.groups, id)
		done = append(done, grp.event(false))
	}
	return done, true
}

// flush finishes every request whose first line is older than the timeout,
// marking it incomplete: a request that never logs "Completed" (the process
// died, the line was lost) should still appear, not vanish.
func (g *railsGrouper) flush(now time.Time) []groupedEvent {
	var out []groupedEvent
	for id, grp := range g.groups {
		if now.Sub(grp.first) >= g.timeout {
			delete(g.groups, id)
			out = append(out, grp.event(true))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].meta.Received.Before(out[j].meta.Received) })
	return out
}

// flushAll finishes everything, for shutdown.
func (g *railsGrouper) flushAll() []groupedEvent {
	out := make([]groupedEvent, 0, len(g.groups))
	for id, grp := range g.groups {
		delete(g.groups, id)
		out = append(out, grp.event(true))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].meta.Received.Before(out[j].meta.Received) })
	return out
}

func (g *railsGrouper) evictOldest() []groupedEvent {
	var oldest *railsGroup
	for _, grp := range g.groups {
		if oldest == nil || grp.first.Before(oldest.first) {
			oldest = grp
		}
	}
	if oldest == nil {
		return nil
	}
	delete(g.groups, oldest.id)
	return []groupedEvent{oldest.event(true)}
}

// groupedEvent is a finished request: fields for the remappers and the meta
// of its first line (its receive time stands in when the line has none).
type groupedEvent struct {
	fields map[string]any
	meta   Meta
}

func (grp *railsGroup) absorb(rest string) {
	f := grp.fields
	switch {
	case railsStarted.MatchString(rest):
		for k, v := range named(railsStarted, rest) {
			if k == "path" {
				path, query, _ := strings.Cut(v.(string), "?")
				f["path"] = path
				if query != "" {
					f["query"] = query
				}
				continue
			}
			f[k] = v
		}
	case railsProcessing.MatchString(rest):
		for k, v := range named(railsProcessing, rest) {
			f[k] = v
		}
	case railsParams.MatchString(rest):
		f["params"] = railsParams.FindStringSubmatch(rest)[1]
	case railsCompleted.MatchString(rest):
		c := named(railsCompleted, rest)
		code, _ := strconv.Atoi(c["status"].(string))
		f["status_code"] = int64(code)
		ms, _ := strconv.ParseFloat(c["ms"].(string), 64)
		f["duration"] = ms
		if r, ok := c["reason"]; ok {
			f["reason"] = r
		}
		if t, ok := c["timings"].(string); ok {
			for _, tm := range railsTiming.FindAllStringSubmatch(t, -1) {
				if v, err := strconv.ParseFloat(tm[2], 64); err == nil {
					key := strings.ToLower(tm[1])
					if key == "views" || key == "activerecord" {
						key += "_ms"
						if key == "activerecord_ms" {
							key = "active_record_ms"
						}
					}
					f[key] = v
				}
			}
		}
		grp.done = true
	case railsException.MatchString(rest):
		if _, seen := f["error_class"]; !seen {
			e := named(railsException, rest)
			f["error_class"] = e["class"]
			if m, ok := e["message"]; ok {
				f["error_message"] = m
			}
		}
	}
}

// event turns the collected fields into a groupedEvent.
func (grp *railsGroup) event(incomplete bool) groupedEvent {
	f := grp.fields
	if incomplete {
		f["incomplete"] = true
	}
	msg := strings.TrimSpace(strings.Join([]string{asString(f["method"]), asString(f["path"])}, " "))
	if code, ok := f["status_code"]; ok {
		msg += " " + strconv.FormatInt(code.(int64), 10)
	} else if msg == "" {
		msg = "request " + grp.id
	}
	f["message"] = msg
	if code, ok := f["status_code"].(int64); ok {
		switch {
		case code >= 500:
			f["level"] = "error"
		case code >= 400:
			f["level"] = "warn"
		default:
			f["level"] = "info"
		}
	} else if incomplete {
		f["level"] = "warn"
	}
	if _, ok := f["error_class"]; ok {
		f["level"] = "error"
	}
	return groupedEvent{fields: f, meta: grp.meta}
}

func named(re *regexp.Regexp, s string) map[string]any {
	m := re.FindStringSubmatch(s)
	out := map[string]any{}
	for i, n := range re.SubexpNames() {
		if i > 0 && n != "" && m[i] != "" {
			out[n] = m[i]
		}
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
