package logpipeline

import (
	"encoding/json"
	"testing"
)

func TestNormalizeStatus(t *testing.T) {
	for in, want := range map[any]string{
		"INFO": "info", "info": "info", "Info": "info", " warn ": "warn", "WARNING": "warn", "Warning": "warn",
		"error": "error", "ERR": "error", "fatal": "critical", "CRITICAL": "critical", "panic": "critical",
		"trace": "debug", "DEBUG": "debug", "verbose": "debug", "notice": "info", "silly": "debug",
		// syslog numbers, as numbers and as numeric strings
		0: "critical", 1: "critical", 2: "critical", 3: "error", 4: "warn", 5: "info", 6: "info", 7: "debug",
		"3": "error", json.Number("4"): "warn", float64(6): "info",
		// pino / bunyan
		10: "debug", 20: "debug", 30: "info", 40: "warn", 50: "error", 60: "critical",
		// the ends of each range, so the boundaries are pinned from both sides
		11: "debug", 21: "info", 31: "warn", 41: "error", 51: "critical", 59: "critical",
		json.Number("30"): "info", json.Number("50"): "error",
	} {
		got, ok := normalizeStatus(in)
		if !ok || got != want {
			t.Errorf("normalizeStatus(%#v) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []any{"", " ", "loud", "informative", 8, 9, 61, 100, -1, 3.5, json.Number("2.5"), nil, true, []any{"info"}} {
		if got, ok := normalizeStatus(in); ok {
			t.Errorf("normalizeStatus(%#v) = %q; want it refused", in, got)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	const want = int64(1_790_000_000_123)
	for name, in := range map[string]any{
		"rfc3339 ms":         "2026-09-21T14:13:20.123Z",
		"rfc3339 offset":     "2026-09-21T16:13:20.123+02:00",
		"rfc3339 ns":         "2026-09-21T14:13:20.123000000Z",
		"no zone is utc":     "2026-09-21T14:13:20.123",
		"space":              "2026-09-21 14:13:20.123",
		"python asctime":     "2026-09-21 14:13:20,123",
		"space with zone":    "2026-09-21 14:13:20.123Z",
		"epoch ms":           json.Number("1790000000123"),
		"epoch ms float64":   float64(1790000000123),
		"epoch ms string":    "1790000000123",
		"epoch s fractional": json.Number("1790000000.123"),
		"epoch us":           json.Number("1790000000123000"),
		"epoch ns":           json.Number("1790000000123000000"),
	} {
		got, ok := parseTimestamp(in)
		if !ok || got != want {
			t.Errorf("%s: parseTimestamp(%#v) = %d, %v; want %d", name, in, got, ok, want)
		}
	}
	if got, ok := parseTimestamp("2026-09-21T14:13:20Z"); !ok || got != 1_790_000_000_000 {
		t.Errorf("whole seconds = %d, %v", got, ok)
	}
	// Each unit's edges: just below is the unit before, the edge itself is the unit.
	for in, want := range map[string]int64{
		"1000000000":         1_000_000_000_000, // 1e9: the smallest plausible seconds value
		"99999999999":        99_999_999_999_000,
		"100000000000":       100_000_000_000, // 1e11: milliseconds begin
		"99999999999999":     99_999_999_999_999,
		"100000000000000":    100_000_000_000,    // 1e14: microseconds begin
		"99999999999999000":  99_999_999_999_999, // the largest value float64 keeps below 1e17
		"100000000000000000": 100_000_000_000,    // 1e17: nanoseconds begin
	} {
		if got, ok := parseTimestamp(json.Number(in)); !ok || got != want {
			t.Errorf("parseTimestamp(%s) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := parseTimestamp(json.Number("999999999")); ok {
		t.Error("999999999 (before 2001 in seconds) was accepted")
	}
	for _, in := range []any{"", "yesterday", "12:00:00", "2026-13-45T00:00:00Z", json.Number("5"), json.Number("123456"), float64(-5), 0.0, json.Number("1e30"), nil, true} {
		if got, ok := parseTimestamp(in); ok {
			t.Errorf("parseTimestamp(%#v) = %d; want it refused (not a plausible log time)", in, got)
		}
	}
}

func TestNormalizeTraceAndSpanIDs(t *testing.T) {
	const hex32 = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct {
		in      any
		decimal bool
		want    string
		ok      bool
	}{
		{hex32, false, hex32, true},
		{"0123456789ABCDEF0123456789ABCDEF", false, hex32, true},
		{"0123456789abcdef", false, "00000000000000000123456789abcdef", true}, // 64-bit hex, padded
		{"1234567890123456", false, "00000000000000001234567890123456", true}, // 16 digits are valid hex...
		{"1234567890123456", true, "0000000000000000000462d53c8abac0", true},  // ...and decimal: only the key decides
		{"1", true, "00000000000000000000000000000001", true},
		{"18446744073709551615", true, "0000000000000000ffffffffffffffff", true}, // max uint64
		{"18446744073709551616", true, "", false},                                // overflows 64 bits
		{"0", true, "", false},
		{"abc", false, "", false}, {"xyz", true, "", false}, {"", false, "", false}, {hex32 + "0", false, "", false},
		{json.Number("12345"), true, "00000000000000000000000000003039", true},
		{nil, false, "", false}, {42.0, false, "", false},
	} {
		got, ok := normalizeTraceID(c.in, c.decimal)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeTraceID(%#v, decimal=%v) = %q, %v; want %q, %v", c.in, c.decimal, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		in      any
		decimal bool
		want    string
		ok      bool
	}{
		{"0123456789abcdef", false, "0123456789abcdef", true},
		{"0123456789ABCDEF", false, "0123456789abcdef", true},
		{"123456789abcdef", false, "", false}, // 15 characters
		{"1234567890123456", true, "000462d53c8abac0", true},
		{"255", true, "00000000000000ff", true},
		{"zz", true, "", false}, {"", true, "", false},
	} {
		got, ok := normalizeSpanID(c.in, c.decimal)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeSpanID(%#v, decimal=%v) = %q, %v; want %q, %v", c.in, c.decimal, got, ok, c.want, c.ok)
		}
	}
}
