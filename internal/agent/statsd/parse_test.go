package statsd

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func tagsOf(m Message) []string {
	var out []string
	m.EachTag(func(t []byte) { out = append(out, string(t)) })
	return out
}

func TestParse_EveryType(t *testing.T) {
	for _, tc := range []struct {
		line string
		typ  Type
		val  float64
	}{
		{"a:1|c", Counter, 1},
		{"a:-2.5|g", Gauge, -2.5},
		{"a:12.4|h", Histogram, 12.4},
		{"a:250|ms", Timing, 250},
		{"a:3|d", Distribution, 3},
		{"a:1e3|g", Gauge, 1000},
		{"a:+4|g", Gauge, 4},
	} {
		m, err := Parse([]byte(tc.line))
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if string(m.Name) != "a" || m.Type != tc.typ || m.Value != tc.val || m.SampleRate != 1 || m.Tags != nil || m.Timestamp != 0 {
			t.Errorf("%s: got %+v", tc.line, m)
		}
	}
}

func TestParse_SetKeepsTheRawMember(t *testing.T) {
	m, err := Parse([]byte("users.unique:user_91|s|#a:b"))
	if err != nil || m.Type != Set || string(m.SetMember) != "user_91" || m.Value != 0 {
		t.Fatalf("got %+v, %v", m, err)
	}
	// A member that looks like garbage as a number is fine for a set.
	if _, err := Parse([]byte("u:NaN|s")); err != nil {
		t.Fatal(err)
	}
}

func TestParse_OptionalSectionsInAnyOrder(t *testing.T) {
	for _, line := range []string{
		"page.views:1|c|@0.5|#route:/,method:get|T1790000000",
		"page.views:1|c|#route:/,method:get|T1790000000|@0.5",
		"page.views:1|c|T1790000000|@0.5|#route:/,method:get",
		"page.views:1|c|c:abc123|@0.5|#route:/,method:get|T1790000000", // |c: container id: ignored
		"page.views:1|c|@0.5||#route:/,method:get|T1790000000",         // empty section: ignored
	} {
		m, err := Parse([]byte(line))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if m.SampleRate != 0.5 || m.Timestamp != 1790000000 || strings.Join(tagsOf(m), " ") != "route:/ method:get" {
			t.Errorf("%s: got %+v tags=%q", line, m, tagsOf(m))
		}
	}
}

func TestParse_MalformedLines(t *testing.T) {
	for _, tc := range []struct{ line, reason string }{
		{"", "missing name"},
		{":1|c", "missing name"},
		{"noval", "missing name"},
		{"a:1", "missing '|<type>'"},
		{"a:|c", "empty value"},
		{"a:1|x", "unknown type"},
		{"a:1|", "unknown type"},
		{"a:1|cc", "unknown type"},
		{"a:abc|c", "not a number"},
		{"a:1:2|c", "not a number"}, // packed values are not supported
		{"a:NaN|g", "not finite"},
		{"a:Inf|g", "not finite"},
		{"a:-Inf|h", "not finite"},
		{"a:1e999|g", "not a number"},
		{"a:1|c|@0", "sample rate"},
		{"a:1|c|@1.5", "sample rate"},
		{"a:1|c|@-1", "sample rate"},
		{"a:1|c|@x", "sample rate"},
		{"a:1|c|@NaN", "sample rate"},
		// In (0,1] but not usable: 1/1e-320 is +Inf, so the scaled value is
		// too, and an +Inf in a bucket never comes back out.
		{"a:1|c|@1e-320", "too small to scale by"},
		{"a:1|c|Tx", "timestamp"},
		{"a:1|c|T-5", "timestamp"},
		{"a:1|c|T1.5", "timestamp"},
	} {
		_, err := Parse([]byte(tc.line))
		var pe *ParseError
		if !errors.As(err, &pe) || !strings.Contains(pe.Reason, tc.reason) {
			t.Errorf("Parse(%q) = %v, want a ParseError about %q", tc.line, err, tc.reason)
		}
	}
}

func TestParse_EventsAndServiceChecksAreRecognized(t *testing.T) {
	if _, err := Parse([]byte("_e{5,4}:title|text|#a:b")); !errors.Is(err, ErrEvent) {
		t.Errorf("event: %v", err)
	}
	if _, err := Parse([]byte("_sc|my.check|0|#a:b")); !errors.Is(err, ErrServiceCheck) {
		t.Errorf("service check: %v", err)
	}
}

func TestLines_SplitsOnNewlinesAndSkipsBlanks(t *testing.T) {
	var got []string
	Lines([]byte("a:1|c\nb:2|g\r\n\n\nc:3|h\n"), func(l []byte) { got = append(got, string(l)) })
	if strings.Join(got, " ") != "a:1|c b:2|g c:3|h" {
		t.Fatalf("got %q", got)
	}
	Lines(nil, func([]byte) { t.Fatal("called for empty datagram") })
}

func TestEachTag_SkipsEmpty(t *testing.T) {
	m := Message{Tags: []byte(",a:b,,c,")}
	if got := strings.Join(tagsOf(m), " "); got != "a:b c" {
		t.Fatalf("got %q", got)
	}
}

func TestType_String(t *testing.T) {
	if Counter.String() != "c" || Timing.String() != "ms" || Type(0).String() != "Type(0)" || Type(99).String() != "Type(99)" {
		t.Fatal("Type.String wrong")
	}
}

// --- shared goldens (L7) ---------------------------------------------------

type sdkCase struct {
	Name         string   `json:"name"`
	Init         sdkInit  `json:"init"`
	Call         string   `json:"call"`
	Metric       string   `json:"metric"`
	Value        any      `json:"value"`
	ValueSpecial string   `json:"value_special"`
	Tags         []string `json:"tags"`
	SampleRate   *float64 `json:"sample_rate"`
	Random       *float64 `json:"random"`
	Expect       *string  `json:"expect"`
}

type sdkInit struct {
	Service, Env, Version string
	Tags                  []string
}

// TestAppendCanonicalFloat pins §A's number form at the thresholds, which is
// where the three implementations used to disagree: strconv's 'g' reaches for
// an exponent at 1e6, JavaScript's String() not until 1e21, and Python's repr
// at 1e16 — the one this form follows.
func TestAppendCanonicalFloat(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0"}, {math.Copysign(0, -1), "0"}, {1, "1"}, {-1, "-1"}, {1.5, "1.5"},
		{0.30000000000000004, "0.30000000000000004"}, // Go folds 0.1+0.2 exactly, so write it out
		{1048576, "1048576"},                         // 'g' wrote 1.048576e+06
		{1e6, "1000000"},                             // 'g' wrote 1e+06
		{1e15, "1000000000000000"},                   // positional right up to the threshold
		{1e16, "1e+16"},                              // and exponent from it
		{1e20, "1e+20"},                              // String() would write the digits out
		{1e21, "1e+21"},
		{0.0001, "0.0001"}, // positional down to 1e-4
		{0.00001, "1e-05"}, // String() would write 0.00001
		{1e-7, "1e-07"},    // String() would write 1e-7
		{5e-324, "5e-324"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
	} {
		if got := string(appendCanonicalFloat(nil, tc.v)); got != tc.want {
			t.Errorf("appendCanonicalFloat(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

// The SDK contract file is also a contract for this parser: every line an
// SDK is required to send must parse back to exactly the call that made it.
// This checks the golden file against the spec's rules as much as it checks
// the parser.
func TestParse_SDKGoldensRoundTripToTheirCalls(t *testing.T) {
	data, err := os.ReadFile("../../../pkg/wire/testdata/statsd/sdk-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Cases []sdkCase }
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 20 {
		t.Fatalf("only %d cases loaded", len(f.Cases))
	}
	sanitize := func(s string, extra string) string {
		return strings.NewReplacer("|", "_", ",", "_", "\n", "_", extra, "_").Replace(s)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Expect == nil {
				if c.ValueSpecial == "" && (c.SampleRate == nil || c.Random == nil || *c.Random < *c.SampleRate) {
					t.Fatal("expect is null but nothing in the case explains a drop")
				}
				return
			}
			m, err := Parse([]byte(*c.Expect))
			if err != nil {
				t.Fatalf("golden line %q does not parse: %v", *c.Expect, err)
			}
			if want := sanitize(c.Metric, ":"); string(m.Name) != want {
				t.Errorf("name %q, want %q", m.Name, want)
			}
			wantType := map[string]Type{"increment": Counter, "decrement": Counter, "gauge": Gauge, "histogram": Histogram,
				"distribution": Distribution, "timing": Timing, "set": Set}[c.Call]
			if m.Type != wantType {
				t.Errorf("type %v, want %v", m.Type, wantType)
			}
			switch c.Call {
			case "set":
				want := sanitize(fmt.Sprint(c.Value), "\n")
				if string(m.SetMember) != want {
					t.Errorf("member %q, want %q", m.SetMember, want)
				}
			default:
				want := 1.0
				if v, ok := c.Value.(float64); ok {
					want = v
				}
				if c.Call == "decrement" {
					want = -want
				}
				if m.Value != want {
					t.Errorf("value %v, want %v", m.Value, want)
				}
			}
			// The canonical number form is a three-way contract: both SDKs
			// write these bytes and so must this package's own writer, or
			// "the canonical client format of §A" names three formats.
			// Parsing the golden back only compares floats, which is why
			// AppendMessage could disagree with the SDKs unnoticed.
			if c.Call != "set" {
				line := *c.Expect
				text := line[strings.IndexByte(line, ':')+1 : strings.IndexByte(line, '|')]
				if got := string(appendCanonicalFloat(nil, m.Value)); got != text {
					t.Errorf("AppendMessage writes %q for this value; the golden says %q", got, text)
				}
			}
			wantRate := 1.0
			if c.SampleRate != nil {
				wantRate = *c.SampleRate
			}
			if m.SampleRate != wantRate {
				t.Errorf("rate %v, want %v", m.SampleRate, wantRate)
			}
			var wantTags []string
			for _, tg := range c.Tags {
				wantTags = append(wantTags, sanitize(tg, "\n"))
			}
			wantTags = append(wantTags, c.Init.Tags...)
			for _, kv := range [][2]string{{"service", c.Init.Service}, {"env", c.Init.Env}, {"version", c.Init.Version}} {
				if kv[1] != "" {
					wantTags = append(wantTags, kv[0]+":"+kv[1])
				}
			}
			if got := tagsOf(m); strings.Join(got, ",") != strings.Join(wantTags, ",") {
				t.Errorf("tags %q, want %q", got, wantTags)
			}
		})
	}
}

// --- properties (L2) -------------------------------------------------------

func genMessage() *rapid.Generator[Message] {
	return rapid.Custom(func(t *rapid.T) Message {
		name := rapid.StringMatching(`[a-zA-Z][a-zA-Z0-9_.]{0,20}`).Draw(t, "name")
		typ := rapid.SampledFrom([]Type{Counter, Gauge, Set, Histogram, Timing, Distribution}).Draw(t, "type")
		m := Message{Name: []byte(name), Type: typ, SampleRate: 1}
		if typ == Set {
			m.SetMember = []byte(rapid.StringMatching(`[^|\n:]{1,12}`).Draw(t, "member"))
		} else {
			m.Value = rapid.Float64().Filter(func(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }).Draw(t, "value")
		}
		if rapid.Bool().Draw(t, "sampled") {
			m.SampleRate = rapid.Float64Range(0.001, 1).Draw(t, "rate")
		}
		if rapid.Bool().Draw(t, "tagged") {
			m.Tags = []byte(rapid.StringMatching(`[a-z][a-z0-9:/._-]{0,10}(,[a-z][a-z0-9:/._-]{0,10}){0,4}`).Draw(t, "tags"))
		}
		if rapid.Bool().Draw(t, "timestamped") {
			m.Timestamp = rapid.Int64Range(1, 1<<34).Draw(t, "ts")
		}
		return m
	})
}

func TestParse_InvertsAppendMessage(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := genMessage().Draw(t, "m")
		line := AppendMessage(nil, m)
		got, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(%q): %v", line, err)
		}
		if string(got.Name) != string(m.Name) || got.Type != m.Type || got.Value != m.Value ||
			string(got.SetMember) != string(m.SetMember) || got.SampleRate != m.SampleRate ||
			string(got.Tags) != string(m.Tags) || got.Timestamp != m.Timestamp {
			t.Fatalf("round trip of %q:\n got %+v\nwant %+v", line, got, m)
		}
	})
}

// --- fuzz (L4) -------------------------------------------------------------

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"a:1|c", "a:1|c|@0.5|#a:b,c|T123", "u:x|s", "a:1|ms|c:abc", "_e{1,1}:a|b", "_sc|x|0",
		"a:1e999|g", "a:NaN|g", "::||", "a:1|c|@", "a:1|c|#", "a:1|c|T",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		m, err := Parse(line)
		if err != nil {
			return
		}
		// Whatever parses must be internally consistent and survive a round
		// trip through the canonical format.
		if len(m.Name) == 0 || m.Type == 0 || !(m.SampleRate > 0 && m.SampleRate <= 1) ||
			math.IsNaN(m.Value) || math.IsInf(m.Value, 0) || m.Timestamp < 0 {
			t.Fatalf("Parse(%q) accepted an inconsistent message %+v", line, m)
		}
		again, err := Parse(AppendMessage(nil, m))
		if err != nil {
			t.Fatalf("re-parse of %q failed: %v", AppendMessage(nil, m), err)
		}
		if again.Type != m.Type || again.Value != m.Value || string(again.Name) != string(m.Name) {
			t.Fatalf("round trip changed %+v into %+v", m, again)
		}
	})
}

// --- benchmarks (L12) ------------------------------------------------------

var sink Message

func BenchmarkParse(b *testing.B) {
	line := []byte("http.request.count:1|c|@0.5|#service:shop,env:dev,route:/api/comics,method:get,status:200")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		m, err := Parse(line)
		if err != nil {
			b.Fatal(err)
		}
		sink = m
	}
}
