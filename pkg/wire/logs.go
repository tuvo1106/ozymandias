package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Limits on a /v1/logs body (§E).
const (
	// MaxLogsPerRequest bounds one body; the agent's batcher splits.
	MaxLogsPerRequest = 1000
	// MaxMessageBytes bounds one message. Longer is truncated, not refused: a
	// log line is evidence, and losing the end of a 300 KiB stack trace is
	// better than losing the line.
	MaxMessageBytes = 256 << 10
	// MaxAttrsBytes bounds one log's attrs as serialized. Over it the log is
	// refused whole, because there is no honest way to drop part of an object.
	MaxAttrsBytes = 64 << 10
	// MaxLabelLen bounds service, source and host. They become stream labels
	// (docs/plan/M4-logs.md §3), and a label is meant to be a short name.
	MaxLabelLen = 200
)

// Log statuses, lowest to highest (§E). The agent's pipeline maps whatever
// an app says (levelname, severity, syslog numbers) onto these five, so the
// store and the UI never see a sixth.
const (
	StatusDebug    = "debug"
	StatusInfo     = "info"
	StatusWarn     = "warn"
	StatusError    = "error"
	StatusCritical = "critical"
)

var logStatusRank = map[string]int{StatusDebug: 0, StatusInfo: 1, StatusWarn: 2, StatusError: 3, StatusCritical: 4}

// ValidLogStatus reports whether s is one of the five normalized statuses.
func ValidLogStatus(s string) bool { _, ok := logStatusRank[s]; return ok }

// LogStatusRank orders statuses (debug lowest) and is -1 for an unknown one,
// so a "status:>=warn" comparison needs no string table of its own.
func LogStatusRank(s string) int {
	if r, ok := logStatusRank[s]; ok {
		return r
	}
	return -1
}

// Log is one log event on the wire (§E).
//
// Attrs holds numbers as [json.Number], not float64: an order id above 2^53
// is a legitimate attribute and must come out of the store the way it went
// in. The same applies to every consumer that decodes a stored log.
type Log struct {
	Ts      int64          `json:"ts"` // unix milliseconds
	Message string         `json:"message"`
	Status  string         `json:"status"`
	Service string         `json:"service"`
	Source  string         `json:"source,omitempty"`
	Host    string         `json:"host,omitempty"`
	Tags    []string       `json:"tags,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	TraceID string         `json:"trace_id,omitempty"`
	SpanID  string         `json:"span_id,omitempty"`
}

// LogsPayload is a /v1/logs request body.
type LogsPayload struct {
	Logs []Log `json:"logs"`
}

// LogRejection says why one log in a payload was refused.
type LogRejection struct {
	Index  int
	Reason string
}

func (r LogRejection) String() string { return fmt.Sprintf("logs[%d]: %s", r.Index, r.Reason) }

// DecodeLogs parses a /v1/logs body and validates each log on its own, the
// way [DecodeSeries] does for series: the valid ones come back normalized
// (canonical tags, a message cut to MaxMessageBytes and flagged), the invalid
// ones as rejections, and the error is reserved for a body that is not a logs
// payload at all. A log is refused whole or accepted whole, so a sender can
// predict what happened to each line from its rules alone.
func DecodeLogs(body []byte, opts DecodeOptions) ([]Log, []LogRejection, error) {
	var raw struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("body is not a logs payload: %w", err)
	}
	if raw.Logs == nil {
		return nil, nil, errors.New(`body has no "logs" array`)
	}
	if len(raw.Logs) > MaxLogsPerRequest {
		return nil, nil, fmt.Errorf("%d logs in one request; the limit is %d", len(raw.Logs), MaxLogsPerRequest)
	}
	valid := make([]Log, 0, len(raw.Logs))
	var rejects []LogRejection
	for i, item := range raw.Logs {
		var l Log
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.UseNumber()
		if err := dec.Decode(&l); err != nil {
			rejects = append(rejects, LogRejection{Index: i, Reason: err.Error()})
			continue
		}
		if err := ValidateLog(&l, opts); err != nil {
			rejects = append(rejects, LogRejection{Index: i, Reason: err.Error()})
			continue
		}
		l.Tags = CanonicalTags(l.Tags)
		truncateMessage(&l)
		valid = append(valid, l)
	}
	return valid, rejects, nil
}

// ValidateLog checks one log against §E. It does not modify l, so a message
// over the limit passes: truncation is a normalization, not a refusal.
func ValidateLog(l *Log, opts DecodeOptions) error {
	if l.Ts <= 0 {
		return fmt.Errorf("ts %d is not a positive unix time in milliseconds", l.Ts)
	}
	nowMs := opts.Now.UnixMilli()
	if latest := nowMs + MaxFutureSkew.Milliseconds(); l.Ts > latest {
		return fmt.Errorf("ts %d is more than %s in the future (is it in milliseconds?)", l.Ts, MaxFutureSkew)
	}
	if l.Ts < 1_000_000_000_000 {
		return fmt.Errorf("ts %d looks like seconds; want unix milliseconds", l.Ts)
	}
	if opts.MaxAge > 0 && l.Ts < nowMs-opts.MaxAge.Milliseconds() {
		return fmt.Errorf("ts %d is older than the accepted window (%s)", l.Ts, opts.MaxAge)
	}
	if !ValidLogStatus(l.Status) {
		return fmt.Errorf("unknown status %q (want debug, info, warn, error or critical)", l.Status)
	}
	if l.Service == "" {
		return errors.New("service is required")
	}
	for name, v := range map[string]string{"service": l.Service, "source": l.Source, "host": l.Host} {
		if err := validLabel(name, v); err != nil {
			return err
		}
	}
	if len(l.Tags) > MaxTagsPerPoint {
		return fmt.Errorf("%d tags; the limit is %d", len(l.Tags), MaxTagsPerPoint)
	}
	for _, t := range l.Tags {
		if !ValidTag(t) {
			return fmt.Errorf("invalid tag %q", t)
		}
	}
	if l.TraceID != "" && !isLowerHex(l.TraceID, 32) {
		return errors.New("trace_id must be 32 lowercase hex characters")
	}
	if l.SpanID != "" && !isLowerHex(l.SpanID, 16) {
		return errors.New("span_id must be 16 lowercase hex characters")
	}
	if len(l.Attrs) > 0 {
		b, err := json.Marshal(l.Attrs)
		if err != nil {
			return fmt.Errorf("attrs do not serialize: %w", err)
		}
		limit := MaxAttrsBytes
		if len(l.Message) > MaxMessageBytes {
			// Truncating will add attrs._truncated; leave room for it, so
			// what DecodeLogs returns still passes this check.
			limit -= len(truncatedFlag)
		}
		if len(b) > limit {
			return fmt.Errorf("attrs are %d bytes serialized; the limit is %d", len(b), limit)
		}
	}
	return nil
}

func validLabel(name, v string) error {
	if len(v) > MaxLabelLen {
		return fmt.Errorf("%s is %d bytes; the limit is %d", name, len(v), MaxLabelLen)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isDigit(c) && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// truncatedFlag is what truncation adds to the serialized attrs.
const truncatedFlag = `,"_truncated":true`

// truncateMessage cuts l.Message to MaxMessageBytes on a rune boundary and
// sets attrs._truncated, so a reader can tell a short log from a cut one.
func truncateMessage(l *Log) {
	if len(l.Message) <= MaxMessageBytes {
		return
	}
	cut := MaxMessageBytes
	for cut > 0 && !utf8.RuneStart(l.Message[cut]) {
		cut--
	}
	l.Message = l.Message[:cut]
	if l.Attrs == nil {
		l.Attrs = map[string]any{}
	}
	l.Attrs["_truncated"] = true
}

func validUTF8(s string) bool { return utf8.ValidString(s) }
