package tailer

import (
	"regexp"
	"strings"
	"time"
)

// Multiline limits and timing. A traceback is one event; the limits keep one
// runaway event (a loop printing frames) from growing without bound.
const (
	// MaxMultilineLines and MaxMultilineBytes cap one event.
	MaxMultilineLines = 500
	MaxMultilineBytes = 256 << 10
	// MultilineFlushAfter is how long a pending event waits for a
	// continuation before it is emitted: a log that ends with a traceback
	// must not wait for the next line to appear.
	MultilineFlushAfter = time.Second
)

// event is one logical log entry: one line, or several joined by multiline.
type event struct {
	text string
	// end is the offset just past its last line (file) or the timestamp of its
	// last line (docker): what may be committed once it is delivered.
	end int64
	// stderr is whether the event's first line came from stderr.
	stderr bool
}

// multiline joins lines into events: a line matching start begins a new
// event, any other line continues the pending one. Python tracebacks are the
// case it exists for — "Traceback…" and every frame after the ERROR line
// belong to it, and only the ERROR line matches the start pattern.
type multiline struct {
	start   *regexp.Regexp // nil: every line is its own event
	lines   []string
	bytes   int
	end     int64
	touched time.Time
	stderr  bool
}

func newMultiline(start *regexp.Regexp) *multiline { return &multiline{start: start} }

// add takes one line and returns the events it completes (zero or one, or two
// when the line is itself over the caps).
func (m *multiline) add(text string, end int64, now time.Time, stderr bool) []event {
	if m.start == nil {
		return []event{{text: text, end: end, stderr: stderr}}
	}
	var out []event
	startsNew := m.start.MatchString(text)
	if len(m.lines) > 0 && (startsNew || len(m.lines) >= MaxMultilineLines || m.bytes+len(text) > MaxMultilineBytes) {
		out = append(out, m.take())
	}
	if len(m.lines) == 0 {
		m.stderr = stderr
	}
	m.lines = append(m.lines, text)
	m.bytes += len(text) + 1
	m.end, m.touched = end, now
	return out
}

// flush emits the pending event if nothing has joined it for
// MultilineFlushAfter (or always, if force).
func (m *multiline) flush(now time.Time, force bool) []event {
	if len(m.lines) == 0 || (!force && now.Sub(m.touched) < MultilineFlushAfter) {
		return nil
	}
	return []event{m.take()}
}

func (m *multiline) take() event {
	e := event{text: strings.Join(m.lines, "\n"), end: m.end, stderr: m.stderr}
	m.lines, m.bytes = nil, 0
	return e
}

func (m *multiline) reset() { m.lines, m.bytes = nil, 0 }
