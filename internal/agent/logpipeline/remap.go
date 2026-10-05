package logpipeline

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// normalizeStatus maps whatever an app calls a severity onto the five wire
// statuses. It accepts names in any case (INFO, Warning, fatal), syslog
// numbers (0 emergency .. 7 debug), and the 10..60 numbers pino and bunyan
// use. It reports false for a value it does not recognize, so a caller can
// fall back to another field or a default instead of inventing a level.
func normalizeStatus(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" {
			return "", false
		}
		if n, err := strconv.Atoi(s); err == nil {
			return statusFromNumber(n)
		}
		if st, ok := statusNames[s]; ok {
			return st, true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return statusFromNumber(int(n))
		}
	case float64:
		if v == math.Trunc(v) {
			return statusFromNumber(int(v))
		}
	case int:
		return statusFromNumber(v)
	}
	return "", false
}

var statusNames = map[string]string{
	"trace": wire.StatusDebug, "debug": wire.StatusDebug, "verbose": wire.StatusDebug, "silly": wire.StatusDebug, "fine": wire.StatusDebug,
	"info": wire.StatusInfo, "notice": wire.StatusInfo, "informational": wire.StatusInfo, "log": wire.StatusInfo, "success": wire.StatusInfo,
	"warn": wire.StatusWarn, "warning": wire.StatusWarn,
	"error": wire.StatusError, "err": wire.StatusError, "exception": wire.StatusError, "severe": wire.StatusError,
	"critical": wire.StatusCritical, "crit": wire.StatusCritical, "fatal": wire.StatusCritical, "alert": wire.StatusCritical,
	"emerg": wire.StatusCritical, "emergency": wire.StatusCritical, "panic": wire.StatusCritical,
	// PostgreSQL's severities beyond the shared names: the lines that follow an
	// ERROR (DETAIL, HINT, STATEMENT, CONTEXT) are information about it, and
	// DEBUG1 to DEBUG5 are debug levels.
	"statement": wire.StatusInfo, "detail": wire.StatusInfo, "hint": wire.StatusInfo, "context": wire.StatusInfo,
	"debug1": wire.StatusDebug, "debug2": wire.StatusDebug, "debug3": wire.StatusDebug, "debug4": wire.StatusDebug, "debug5": wire.StatusDebug,
	// Redis marks a line with one character: . debug, - verbose, * notice, # warning.
	".": wire.StatusDebug, "-": wire.StatusDebug, "*": wire.StatusInfo, "#": wire.StatusWarn,
}

// statusFromNumber reads a number as a severity. Syslog levels are 0..7 and
// pino/bunyan levels are multiples of ten from 10 to 60; the two ranges do
// not overlap, which is the only reason one function can take both.
func statusFromNumber(n int) (string, bool) {
	switch {
	case n >= 0 && n <= 2:
		return wire.StatusCritical, true // emergency, alert, critical
	case n == 3:
		return wire.StatusError, true
	case n == 4:
		return wire.StatusWarn, true
	case n == 5 || n == 6:
		return wire.StatusInfo, true // notice, informational
	case n == 7:
		return wire.StatusDebug, true
	case n >= 10 && n <= 20:
		return wire.StatusDebug, true // trace, debug
	case n >= 21 && n <= 30:
		return wire.StatusInfo, true
	case n >= 31 && n <= 40:
		return wire.StatusWarn, true
	case n >= 41 && n <= 50:
		return wire.StatusError, true
	case n >= 51 && n <= 60:
		return wire.StatusCritical, true
	}
	return "", false
}

// parseTimestamp reads a log's own timestamp, in ms. It takes RFC 3339 (with
// or without fractional seconds), the space-separated forms Python and many
// loggers print ("2026-10-04 12:00:00,123" included), and epoch numbers in
// seconds, milliseconds, microseconds or nanoseconds, told apart by size: a
// real timestamp in any of the four lands in a different decade of
// magnitude, so no flag is needed. It reports false for anything else, and
// the caller uses the time the line was received.
func parseTimestamp(v any) (int64, bool) {
	switch v := v.(type) {
	case string:
		return parseTimestampString(strings.TrimSpace(v))
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return epochToMs(f)
	case float64:
		return epochToMs(v)
	case int64:
		return epochToMs(float64(v))
	}
	return 0, false
}

// timeLayouts are tried in order. Go accepts a comma as the fractional-second
// separator when parsing, so Python's default asctime ("2026-10-04 12:00:00,123")
// needs no layout of its own.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999", // no zone: read as UTC
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z0700",
	"2006-01-02 15:04:05.999999999 MST", // PostgreSQL: 2026-09-24 02:40:45.365 UTC
	"2006-01-02 15:04:05 -0700",         // Rails: Started ... at 2026-10-04 12:00:00 +0000
	"02 Jan 2006 15:04:05.999999999",    // Redis: 24 Sep 2026 02:45:46.028
}

func parseTimestampString(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return epochToMs(f)
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// epochToMs takes a number of seconds, milliseconds, microseconds or
// nanoseconds since the epoch and returns milliseconds. The unit is read from
// the size: any date from 2001 on is at least 1e9 in seconds, and the same
// instant is a thousand times larger in each finer unit, so the four ranges
// below do not overlap for any plausible log. Anything smaller than 1e9 is
// not a timestamp.
func epochToMs(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, false
	}
	switch {
	case f < 1e9:
		return 0, false // before 2001 in seconds: not a log timestamp
	case f < 1e11:
		return int64(f * 1000), true // seconds, possibly fractional
	case f < 1e14:
		return int64(f), true // milliseconds
	case f < 1e17:
		return int64(f / 1e3), true // microseconds
	case f < 1e20:
		return int64(f / 1e6), true // nanoseconds
	}
	return 0, false
}

// normalizeTraceID returns a 32-character lowercase hex trace id from what an
// app logs. For a hex field (trace_id) that is 32 hex characters in any case,
// or 16 (a 64-bit trace, padded on the left). For a decimal field (Datadog's
// dd.trace_id) it is the decimal rendering of a 64-bit id. Which one it is
// comes from the *name* of the field, never from the digits: a 16-digit string
// is a valid hex id and a valid decimal id, and only the key says which.
func normalizeTraceID(v any, decimal bool) (string, bool) {
	s := strings.TrimSpace(stringOf(v))
	if s == "" {
		return "", false
	}
	if decimal {
		return decimalID(s, 32)
	}
	switch {
	case len(s) == 32 && isHex(s):
		return strings.ToLower(s), true
	case len(s) == 16 && isHex(s):
		return strings.Repeat("0", 16) + strings.ToLower(s), true
	}
	return "", false
}

// normalizeSpanID is the 16-character form of normalizeTraceID.
func normalizeSpanID(v any, decimal bool) (string, bool) {
	s := strings.TrimSpace(stringOf(v))
	if s == "" {
		return "", false
	}
	if decimal {
		return decimalID(s, 16)
	}
	if len(s) == 16 && isHex(s) {
		return strings.ToLower(s), true
	}
	return "", false
}

// decimalID renders a decimal 64-bit id as lowercase hex padded to width.
func decimalID(s string, width int) (string, bool) {
	if !isDigits(s) {
		return "", false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return "", false
	}
	h := strconv.FormatUint(n, 16)
	return strings.Repeat("0", width-len(h)) + h, true
}

func stringOf(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return s != ""
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
