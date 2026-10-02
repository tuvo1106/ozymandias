package openmetrics

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func parse(t *testing.T, body string, opts Options) []Family {
	t.Helper()
	fams, err := Parse(strings.NewReader(body), opts)
	if err != nil {
		t.Fatalf("Parse: %v\nbody:\n%s", err, body)
	}
	return fams
}

func TestParse_Productions(t *testing.T) {
	inf, ninf := math.Inf(1), math.Inf(-1)
	for _, tc := range []struct {
		name string
		body string
		opts Options
		want []Family
	}{
		{"empty body", "", Options{}, []Family{}},
		{"bare sample is an unknown family", "up 1\n", Options{}, []Family{
			{Name: "up", Type: TypeUnknown, Samples: []Sample{{Name: "up", Value: 1}}},
		}},
		{"no trailing newline", "up 1", Options{}, []Family{
			{Name: "up", Type: TypeUnknown, Samples: []Sample{{Name: "up", Value: 1}}},
		}},
		{"CRLF line endings", "# TYPE up gauge\r\nup 1\r\n", Options{}, []Family{
			{Name: "up", Type: TypeGauge, Samples: []Sample{{Name: "up", Value: 1}}},
		}},
		{"help, type, labels", "# HELP req Requests\\nserved, a \\\\ b.\n# TYPE req counter\nreq{code=\"200\",path=\"/a\"} 3\n", Options{}, []Family{
			{Name: "req", Type: TypeCounter, Help: "Requests\nserved, a \\ b.", Samples: []Sample{
				{Name: "req", Labels: []Label{{"code", "200"}, {"path", "/a"}}, Value: 3},
			}},
		}},
		{"HELP keeps an unknown escape and a trailing backslash", "# HELP x a\\tb\\\nx 1\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Help: "a\\tb\\", Samples: []Sample{{Name: "x", Value: 1}}},
		}},
		{"type after help, untyped spelling", "# HELP x h\n# TYPE x untyped\nx 1\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Help: "h", Samples: []Sample{{Name: "x", Value: 1}}},
		}},
		{"other comments are ignored", "# a comment\n#no space\n# TYPEX y\nx 1\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1}}},
		}},
		{"blank lines allowed in Prometheus text", "\nx 1\n\n  \ny 2\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1}}},
			{Name: "y", Type: TypeUnknown, Samples: []Sample{{Name: "y", Value: 2}}},
		}},
		{"label escapes, trailing comma, spaces", `x{ a = "q\"uo\\te\nd" , b="",} 1` + "\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{
				{Name: "x", Labels: []Label{{"a", "q\"uo\\te\nd"}}, Value: 1}, // b="" is no label
			}},
		}},
		{"empty label set", "x{} 1\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1}}},
		}},
		{"special values", "a NaN\nb +Inf\nc -Inf\nd 1.5e-3\ne -0\n", Options{}, []Family{
			{Name: "a", Type: TypeUnknown, Samples: []Sample{{Name: "a", Value: math.NaN()}}},
			{Name: "b", Type: TypeUnknown, Samples: []Sample{{Name: "b", Value: inf}}},
			{Name: "c", Type: TypeUnknown, Samples: []Sample{{Name: "c", Value: ninf}}},
			{Name: "d", Type: TypeUnknown, Samples: []Sample{{Name: "d", Value: 1.5e-3}}},
			{Name: "e", Type: TypeUnknown, Samples: []Sample{{Name: "e", Value: 0}}},
		}},
		{"colons in metric names", "job:req:rate5m 2\n", Options{}, []Family{
			{Name: "job:req:rate5m", Type: TypeUnknown, Samples: []Sample{{Name: "job:req:rate5m", Value: 2}}},
		}},
		{"Prometheus timestamp is milliseconds", "x 1 1395066363000\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1, TimestampMs: 1395066363000, HasTimestamp: true}}},
		}},
		{"OpenMetrics timestamp is seconds", "x 1 1395066363.5\n# EOF\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1, TimestampMs: 1395066363500, HasTimestamp: true}}},
		}},
		{"OpenMetrics counter owns _total and _created", "# TYPE r counter\nr_total 5\nr_created 1.7e9\n# EOF\n", Options{}, []Family{
			{Name: "r", Type: TypeCounter, Samples: []Sample{{Name: "r_total", Value: 5}, {Name: "r_created", Value: 1.7e9}}},
		}},
		{"exemplar is skipped", "# TYPE h histogram\nh_bucket{le=\"1\"} 2 # {trace_id=\"abc\"} 0.5 1.0\nh_bucket{le=\"+Inf\"} 2\nh_count 2\nh_sum 1\n# EOF\n", Options{}, []Family{
			{Name: "h", Type: TypeHistogram, Samples: []Sample{
				{Name: "h_bucket", Labels: []Label{{"le", "1"}}, Value: 2},
				{Name: "h_bucket", Labels: []Label{{"le", "+Inf"}}, Value: 2},
				{Name: "h_count", Value: 2},
				{Name: "h_sum", Value: 1},
			}},
		}},
		{"timestamp then exemplar", "x 1 5 # {a=\"b\"} 1\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1, TimestampMs: 5, HasTimestamp: true}}},
		}},
		{"summary, histogram, gaugehistogram, info, stateset suffixes", strings.Join([]string{
			"# TYPE s summary", `s{quantile="0.5"} 1`, "s_sum 2", "s_count 3",
			"# TYPE g gaugehistogram", `g_bucket{le="+Inf"} 1`, "g_gsum 1", "g_gcount 1",
			"# TYPE i info", `i_info{v="1"} 1`,
			"# TYPE st stateset", `st{st="a"} 1`,
			"# UNIT s seconds", "# EOF", ""}, "\n"), Options{}, []Family{
			{Name: "s", Type: TypeSummary, Unit: "seconds", Samples: []Sample{
				{Name: "s", Labels: []Label{{"quantile", "0.5"}}, Value: 1}, {Name: "s_sum", Value: 2}, {Name: "s_count", Value: 3},
			}},
			{Name: "g", Type: TypeGaugeHistogram, Samples: []Sample{
				{Name: "g_bucket", Labels: []Label{{"le", "+Inf"}}, Value: 1}, {Name: "g_gsum", Value: 1}, {Name: "g_gcount", Value: 1},
			}},
			{Name: "i", Type: TypeInfo, Samples: []Sample{{Name: "i_info", Labels: []Label{{"v", "1"}}, Value: 1}}},
			{Name: "st", Type: TypeStateset, Samples: []Sample{{Name: "st", Labels: []Label{{"st", "a"}}, Value: 1}}},
		}},
		// A gauge whose name ends in _count is its own family, not part of a
		// histogram — suffixes only bind to a base of the right type.
		{"suffix binds only to a family of the right type", "# TYPE foo gauge\nfoo 1\nfoo_count 2\n", Options{}, []Family{
			{Name: "foo", Type: TypeGauge, Samples: []Sample{{Name: "foo", Value: 1}}},
			{Name: "foo_count", Type: TypeUnknown, Samples: []Sample{{Name: "foo_count", Value: 2}}},
		}},
		{"Prometheus-style counter named _total", "# TYPE r_total counter\nr_total 5\n", Options{}, []Family{
			{Name: "r_total", Type: TypeCounter, Samples: []Sample{{Name: "r_total", Value: 5}}},
		}},
		{"tab separators", "x\t1\t2\n", Options{}, []Family{
			{Name: "x", Type: TypeUnknown, Samples: []Sample{{Name: "x", Value: 1, TimestampMs: 2, HasTimestamp: true}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parse(t, tc.body, tc.opts)
			if !equalFamilies(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// equalFamilies is reflect.DeepEqual with NaN == NaN.
func equalFamilies(a, b []Family) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		fa, fb := a[i], b[i]
		if fa.Name != fb.Name || fa.Type != fb.Type || fa.Help != fb.Help || fa.Unit != fb.Unit || len(fa.Samples) != len(fb.Samples) {
			return false
		}
		for j := range fa.Samples {
			sa, sb := fa.Samples[j], fb.Samples[j]
			if sa.Name != sb.Name || sa.TimestampMs != sb.TimestampMs || sa.HasTimestamp != sb.HasTimestamp {
				return false
			}
			if !(sa.Value == sb.Value || (math.IsNaN(sa.Value) && math.IsNaN(sb.Value))) {
				return false
			}
			if len(sa.Labels)+len(sb.Labels) > 0 && !reflect.DeepEqual(sa.Labels, sb.Labels) {
				return false
			}
		}
	}
	return true
}

func TestParse_Errors(t *testing.T) {
	small := Limits{MaxSamples: 2, MaxLabels: 2, MaxNameLen: 8, MaxLabelValueLen: 4}
	for _, tc := range []struct {
		name      string
		body      string
		opts      Options
		line, col int
		msg       string
	}{
		{"sample starts with a digit", "1x 1\n", Options{}, 1, 1, "want a metric name"},
		{"no value", "x\n", Options{}, 1, 2, "want a space before the value"},
		{"no value after space", "x{a=\"b\"} \n", Options{}, 1, 10, "want a value"},
		{"bad value", "x abc\n", Options{}, 1, 3, `invalid value "abc"`},
		{"hex value", "x 0x10\n", Options{}, 1, 3, "invalid value"},
		{"underscore value", "x 1_000\n", Options{}, 1, 3, "invalid value"},
		{"bad timestamp", "x 1 soon\n", Options{}, 1, 5, `invalid timestamp "soon"`},
		{"infinite timestamp", "x 1 +Inf\n", Options{}, 1, 5, "invalid timestamp"},
		{"fractional Prometheus timestamp", "x 1 1.5\n", Options{}, 1, 5, "integer milliseconds"},
		{"timestamp out of range", "x 1 1e300\n", Options{}, 1, 5, "out of range"},
		{"junk after timestamp", "x 1 2 3\n", Options{}, 1, 7, `unexpected "3"`},
		{"unterminated labels", "x{a=\"b\"\n", Options{}, 1, 8, "want ',' or '}'"},
		{"label without =", "x{a} 1\n", Options{}, 1, 4, "want '='"},
		{"label value unquoted", "x{a=b} 1\n", Options{}, 1, 5, "want '\"'"},
		{"label name missing", "x{=\"b\"} 1\n", Options{}, 1, 3, "want a label name"},
		{"duplicate label", "x{a=\"1\",a=\"2\"} 1\n", Options{}, 1, 9, `duplicate label "a"`},
		{"invalid escape", "x{a=\"\\t\"} 1\n", Options{}, 1, 6, `invalid escape \t`},
		{"escape at end of line", "x{a=\"\\", Options{}, 1, 6, "unterminated escape"},
		{"unterminated value", "x{a=\"abc\n", Options{}, 1, 5, "unterminated label value"},
		{"second TYPE", "# TYPE x gauge\n# TYPE x counter\n", Options{}, 2, 1, "second TYPE"},
		{"second HELP", "# HELP x a\n# HELP x b\n", Options{}, 2, 1, "second HELP"},
		{"TYPE after samples", "x 1\n# TYPE x gauge\n", Options{}, 2, 1, "after its samples"},
		{"unknown type", "# TYPE x widget\n", Options{}, 1, 10, `unknown type "widget"`},
		{"TYPE without name", "# TYPE \n", Options{}, 1, 8, "without a metric name"},
		{"TYPE with bad name", "# TYPE 9x gauge\n", Options{}, 1, 8, "invalid metric name"},
		{"content after EOF", "x 1\n# EOF\ny 2\n", Options{}, 3, 1, "after # EOF"},
		{"OpenMetrics requires EOF", "x 1\n", Options{Format: FormatOpenMetrics}, 2, 1, "missing # EOF"},
		{"OpenMetrics forbids blank lines", "x 1\n\n# EOF\n", Options{Format: FormatOpenMetrics}, 2, 1, "blank line"},
		{"too many samples", "a 1\nb 2\nc 3\n", Options{Limits: small}, 3, 1, "more than 2 samples"},
		{"too many labels", "x{a=\"1\",b=\"2\",c=\"3\"} 1\n", Options{Limits: small}, 1, 15, "more than 2 labels"},
		{"metric name too long", "abcdefghij 1\n", Options{Limits: small}, 1, 1, "longer than 8"},
		{"HELP name too long", "# HELP abcdefghij x\n", Options{Limits: small}, 1, 8, "longer than 8"},
		{"label name too long", "x{abcdefghij=\"1\"} 1\n", Options{Limits: small}, 1, 3, "label name longer"},
		{"label value too long", "x{a=\"12345\"} 1\n", Options{Limits: small}, 1, 5, "label value longer than 4"},
		{"escaped label value too long", "x{a=\"1\\n2345\"} 1\n", Options{Limits: small}, 1, 5, "label value longer than 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.body), tc.opts)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want a *ParseError", err)
			}
			if pe.Line != tc.line || pe.Col != tc.col || !strings.Contains(pe.Msg, tc.msg) {
				t.Fatalf("got %q (line %d col %d), want line %d col %d containing %q", pe.Error(), pe.Line, pe.Col, tc.line, tc.col, tc.msg)
			}
		})
	}
}

func TestParse_BodyLimit(t *testing.T) {
	_, err := Parse(strings.NewReader(strings.Repeat("x 1\n", 100)), Options{Limits: Limits{MaxBytes: 50}})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// Exactly at the limit is fine.
	if _, err := Parse(strings.NewReader(strings.Repeat("x 1\n", 10)), Options{Limits: Limits{MaxBytes: 40}}); err != nil {
		t.Fatal(err)
	}
}

func TestParse_ReadError(t *testing.T) {
	_, err := Parse(iotest.ErrReader(errors.New("connection reset")), Options{})
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}
}

func TestFormatFromContentType(t *testing.T) {
	for ct, want := range map[string]Format{
		"application/openmetrics-text; version=1.0.0; charset=utf-8": FormatOpenMetrics,
		" Application/OpenMetrics-Text":                              FormatOpenMetrics,
		"text/plain; version=0.0.4; charset=utf-8":                   FormatAuto,
		"": FormatAuto,
	} {
		if got := FormatFromContentType(ct); got != want {
			t.Errorf("%q: got %v, want %v", ct, got, want)
		}
	}
}

func TestSample_Label(t *testing.T) {
	s := Sample{Labels: []Label{{"a", "1"}}}
	if v, ok := s.Label("a"); !ok || v != "1" {
		t.Fatalf("a = %q, %v", v, ok)
	}
	if _, ok := s.Label("b"); ok {
		t.Fatal("b present")
	}
}
