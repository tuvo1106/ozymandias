package statsd

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Type is a statsd metric type (docs/wire-protocol.md §A).
type Type uint8

// Metric types. Timing (ms) is a histogram whose values happen to be
// milliseconds; it is kept distinct only so a formatter can round-trip it.
const (
	Counter      Type = iota + 1 // c
	Gauge                        // g
	Set                          // s
	Histogram                    // h
	Timing                       // ms
	Distribution                 // d
)

var typeCodes = [...]string{Counter: "c", Gauge: "g", Set: "s", Histogram: "h", Timing: "ms", Distribution: "d"}

func (t Type) String() string {
	if t == 0 || int(t) >= len(typeCodes) {
		return fmt.Sprintf("Type(%d)", uint8(t))
	}
	return typeCodes[t]
}

// Message is one parsed statsd line. Its byte slices alias the input line,
// so a Message is valid only until the buffer it was parsed from is reused.
// That is the price of a parser that allocates nothing: the caller must copy
// whatever it keeps (the aggregator copies names and tags only when it sees
// a new context).
type Message struct {
	Name []byte
	// Value is the numeric value, for every type but Set.
	Value float64
	// SetMember is the raw member for Set; nil otherwise.
	SetMember []byte
	Type      Type
	// SampleRate is in (0,1]; 1 when the line has no |@ section.
	SampleRate float64
	// Tags is the raw tag section without the '#': comma-separated, not yet
	// normalized. Iterate it with EachTag.
	Tags []byte
	// Timestamp is the |T section in unix seconds; 0 when absent.
	Timestamp int64
}

// EachTag calls fn for every non-empty comma-separated tag in m.Tags.
func (m *Message) EachTag(fn func(tag []byte)) {
	rest := m.Tags
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, ',')
		if i < 0 {
			i = len(rest)
		}
		if i > 0 {
			fn(rest[:i])
		}
		if i == len(rest) {
			return
		}
		rest = rest[i+1:]
	}
}

// Sentinel errors for lines that are well-formed but are not metrics.
// extended StatsD clients also send events (_e{...}) and service checks (_sc|...)
// over the same socket; ozymandias doesn't store them, but counts them apart
// from garbage so a dashboard can tell "a client sends events" from "a
// client is broken".
var (
	ErrEvent        = errors.New("statsd: events (_e{) are not supported")
	ErrServiceCheck = errors.New("statsd: service checks (_sc|) are not supported")
)

// ParseError describes a malformed line.
type ParseError struct {
	Reason string
}

func (e *ParseError) Error() string { return "statsd: " + e.Reason }

func parseErr(reason string) error { return &ParseError{Reason: reason} }

// Parse parses one statsd line (no trailing newline):
//
//	<name>:<value>|<type>[|@<rate>][|#<tags>][|T<unix_seconds>]
//
// Sections after the type may come in any order; unknown ones (such as the
// |c: container id modern extended StatsD clients add) are ignored, as §0 requires
// of every receiver. Validation of names and tags is left to the aggregator,
// which normalizes rather than rejects.
func Parse(line []byte) (Message, error) {
	var m Message
	if bytes.HasPrefix(line, []byte("_e{")) {
		return m, ErrEvent
	}
	if bytes.HasPrefix(line, []byte("_sc|")) {
		return m, ErrServiceCheck
	}
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return m, parseErr("missing name or ':'")
	}
	m.Name = line[:colon]
	rest := line[colon+1:]

	pipe := bytes.IndexByte(rest, '|')
	if pipe < 0 {
		return m, parseErr("missing '|<type>'")
	}
	value := rest[:pipe]
	rest = rest[pipe+1:]
	if len(value) == 0 {
		return m, parseErr("empty value")
	}

	section, rest := cut(rest)
	switch string(section) { // compiles to a comparison, no allocation
	case "c":
		m.Type = Counter
	case "g":
		m.Type = Gauge
	case "s":
		m.Type = Set
	case "h":
		m.Type = Histogram
	case "ms":
		m.Type = Timing
	case "d":
		m.Type = Distribution
	default:
		return m, parseErr(fmt.Sprintf("unknown type %q", section))
	}

	if m.Type == Set {
		m.SetMember = value
	} else {
		v, err := parseFloat(value)
		if err != nil {
			return m, parseErr(fmt.Sprintf("value %q is not a number", value))
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return m, parseErr(fmt.Sprintf("value %q is not finite", value))
		}
		m.Value = v
	}

	m.SampleRate = 1
	for len(rest) > 0 {
		section, rest = cut(rest)
		if len(section) == 0 {
			continue
		}
		switch section[0] {
		case '@':
			r, err := parseFloat(section[1:])
			if err != nil || !(r > 0 && r <= 1) {
				return m, parseErr(fmt.Sprintf("sample rate %q is not in (0,1]", section[1:]))
			}
			// A rate is used as its reciprocal — the number of samples this
			// one stands for — so a subnormal passes the range check above
			// and still scales the value to +Inf. Symmetric with the value
			// check: in range is not the same as usable.
			if math.IsInf(1/r, 0) {
				return m, parseErr(fmt.Sprintf("sample rate %q is too small to scale by", section[1:]))
			}
			m.SampleRate = r
		case '#':
			m.Tags = section[1:]
		case 'T':
			ts, err := parseInt(section[1:])
			if err != nil || ts < 0 {
				return m, parseErr(fmt.Sprintf("timestamp %q is not unix seconds", section[1:]))
			}
			m.Timestamp = ts
		}
	}
	return m, nil
}

// cut splits b at the first '|'.
func cut(b []byte) (before, after []byte) {
	if i := bytes.IndexByte(b, '|'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

// parseFloat and parseInt convert without copying b to a string: the
// conversion in the call argument doesn't escape, so the compiler skips the
// allocation. strconv's error path would copy it, but that is the cold path.
func parseFloat(b []byte) (float64, error) { return strconv.ParseFloat(string(b), 64) }
func parseInt(b []byte) (int64, error)     { return strconv.ParseInt(string(b), 10, 64) }

// Lines calls fn for every non-empty line in a datagram. Clients separate
// messages with '\n'; a trailing newline and "\r\n" are tolerated.
func Lines(datagram []byte, fn func(line []byte)) {
	for len(datagram) > 0 {
		i := bytes.IndexByte(datagram, '\n')
		var line []byte
		if i < 0 {
			line, datagram = datagram, nil
		} else {
			line, datagram = datagram[:i], datagram[i+1:]
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > 0 {
			fn(line)
		}
	}
}

// AppendMessage appends m in the canonical client format of §A (sections in
// the order type, @rate, #tags, T) — the inverse of Parse, used by tests and
// the load generator.
func AppendMessage(dst []byte, m Message) []byte {
	dst = append(dst, m.Name...)
	dst = append(dst, ':')
	if m.Type == Set {
		dst = append(dst, m.SetMember...)
	} else {
		dst = appendCanonicalFloat(dst, m.Value)
	}
	dst = append(dst, '|')
	dst = append(dst, m.Type.String()...)
	if m.SampleRate != 0 && m.SampleRate != 1 {
		dst = append(dst, "|@"...)
		dst = appendCanonicalFloat(dst, m.SampleRate)
	}
	if len(m.Tags) > 0 {
		dst = append(dst, "|#"...)
		dst = append(dst, m.Tags...)
	}
	if m.Timestamp != 0 {
		dst = append(dst, "|T"...)
		dst = strconv.AppendInt(dst, m.Timestamp, 10)
	}
	return dst
}

// appendCanonicalFloat writes v in §A's canonical number form: the shortest
// decimal that round-trips a float64, positional when the decimal exponent is
// in [-4, 16) and in exponent form (with a signed, at least two-digit
// exponent) outside it. -0 is written as 0.
//
// strconv's 'g' is *not* that form, which is the bug this replaced: with
// shortest precision it switches to an exponent at 1e6, so a byte counter of
// 1048576 went on the wire as "1.048576e+06" while both SDKs wrote
// "1048576". Every form here parses back to the same float64, so nothing was
// being corrupted — but "the canonical client format of §A" was not what this
// function produced, and the round-trip test that compares Parse against it
// could not notice, because it compares floats.
func appendCanonicalFloat(dst []byte, v float64) []byte {
	if v == 0 {
		return append(dst, '0')
	}
	// 'e' with precision -1 gives the shortest round-tripping digits and an
	// exponent of at least two digits — already the canonical exponent form.
	e := strconv.AppendFloat(nil, v, 'e', -1, 64)
	exp, err := strconv.Atoi(string(e[bytes.IndexByte(e, 'e')+1:]))
	if err == nil && (exp < -4 || exp >= 16) {
		return append(dst, e...)
	}
	return strconv.AppendFloat(dst, v, 'f', -1, 64)
}
