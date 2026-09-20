package wire

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

type seriesCase struct {
	Name   string          `json:"name"`
	Reject *string         `json:"reject"`
	Series json.RawMessage `json:"series"`
}

func loadSeriesCases(t *testing.T) (time.Time, []seriesCase) {
	t.Helper()
	data, err := os.ReadFile("testdata/series/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Now   int64        `json:"now"`
		Cases []seriesCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return time.Unix(f.Now, 0), f.Cases
}

// L7 golden: all cases in one payload; exactly the invalid ones are
// rejected, each for its stated reason, and the valid ones survive.
func TestDecodeSeries_Goldens(t *testing.T) {
	now, cases := loadSeriesCases(t)
	items := make([]string, len(cases))
	for i, c := range cases {
		items[i] = string(c.Series)
	}
	body := `{"series":[` + strings.Join(items, ",") + `]}`

	valid, rejects, err := DecodeSeries([]byte(body), DecodeOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	byIndex := map[int]Rejection{}
	for _, r := range rejects {
		byIndex[r.Index] = r
	}
	nValid := 0
	for i, c := range cases {
		r, rejected := byIndex[i]
		switch {
		case c.Reject == nil && rejected:
			t.Errorf("%s: rejected (%s), want accepted", c.Name, r.Reason)
		case c.Reject != nil && !rejected:
			t.Errorf("%s: accepted, want rejected for %q", c.Name, *c.Reject)
		case c.Reject != nil && !strings.Contains(r.Reason, *c.Reject):
			t.Errorf("%s: rejected for %q, want reason containing %q", c.Name, r.Reason, *c.Reject)
		}
		if c.Reject == nil {
			nValid++
		}
	}
	if len(valid) != nValid {
		t.Errorf("%d valid series, want %d", len(valid), nValid)
	}
}

func TestDecodeSeries_CanonicalizesTags(t *testing.T) {
	body := `{"series":[{"metric":"a","type":"gauge","interval":0,"tags":["z:1","a","z:1"],"points":[[100,1]]}]}`
	valid, rejects, err := DecodeSeries([]byte(body), DecodeOptions{Now: time.Unix(100, 0)})
	if err != nil || len(rejects) != 0 || len(valid) != 1 {
		t.Fatalf("valid=%v rejects=%v err=%v", valid, rejects, err)
	}
	if got := strings.Join(valid[0].Tags, ","); got != "a,z:1" {
		t.Fatalf("tags = %s, want a,z:1", got)
	}
}

func TestDecodeSeries_WholeBodyErrors(t *testing.T) {
	many := `{"series":[` + strings.Repeat(`{},`, MaxSeriesPerRequest) + `{}]}`
	for name, body := range map[string]string{
		"not json":      `nope`,
		"no series key": `{"points":[]}`,
		"series null":   `{"series":null}`,
		"series object": `{"series":{}}`,
		"too many":      many,
	} {
		if _, _, err := DecodeSeries([]byte(body), DecodeOptions{Now: time.Unix(100, 0)}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDecodeSeries_EmptyArrayIsFine(t *testing.T) {
	valid, rejects, err := DecodeSeries([]byte(`{"series":[]}`), DecodeOptions{})
	if err != nil || len(valid) != 0 || len(rejects) != 0 {
		t.Fatalf("valid=%v rejects=%v err=%v", valid, rejects, err)
	}
}

func TestValidateSeries_TimeWindowAndLimits(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	base := func() Series {
		return Series{Metric: "a", Type: KindGauge, Points: []Point{{Timestamp: now.Unix(), Value: 1}}}
	}
	ok := base()
	if err := ValidateSeries(&ok, DecodeOptions{Now: now, MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Series){
		"too old":     func(s *Series) { s.Points[0].Timestamp = now.Add(-2 * time.Hour).Unix() },
		"zero ts":     func(s *Series) { s.Points[0].Timestamp = 0 },
		"NaN":         func(s *Series) { s.Points[0].Value = math.NaN() },
		"+Inf":        func(s *Series) { s.Points[0].Value = math.Inf(1) },
		"51 tags":     func(s *Series) { s.Tags = make([]string, 51) },
		"rate, no iv": func(s *Series) { s.Type = KindRate },
	} {
		s := base()
		mutate(&s)
		if err := ValidateSeries(&s, DecodeOptions{Now: now, MaxAge: time.Hour}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPoint_JSON(t *testing.T) {
	b, err := json.Marshal(Point{Timestamp: 1790000000, Value: 12.5})
	if err != nil || string(b) != "[1790000000,12.5]" {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	b, _ = json.Marshal(Point{Timestamp: 1, Value: 1e21})
	if string(b) != "[1,1e+21]" {
		t.Fatalf("Marshal large = %s", b)
	}
	if _, err := json.Marshal(Point{Value: math.NaN()}); err == nil {
		t.Fatal("NaN marshalled")
	}
	var p Point
	for _, bad := range []string{`{}`, `[1]`, `["x",1]`, `[1,"x"]`, `[1.5,1]`} {
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("Unmarshal(%s) accepted", bad)
		}
	}
}

// L2: every finite point survives a JSON round trip exactly.
func TestPoint_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := Point{
			Timestamp: rapid.Int64Range(1, 1<<40).Draw(t, "ts"),
			Value:     rapid.Float64().Draw(t, "v"),
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var got Point
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if got != p && !(got.Timestamp == p.Timestamp && got.Value == 0 && p.Value == 0) {
			t.Fatalf("round trip %v → %s → %v", p, b, got)
		}
	})
}

func TestRejection_String(t *testing.T) {
	if s := (Rejection{Index: 2, Metric: "m", Reason: "bad"}).String(); s != `series[2] "m": bad` {
		t.Fatal(s)
	}
}

// L4: DecodeSeries never panics, and whatever it accepts passes validation
// and re-encodes.
func FuzzDecodeSeries(f *testing.F) {
	f.Add([]byte(`{"series":[{"metric":"a","type":"gauge","interval":0,"tags":["k:v"],"points":[[100,1]]}]}`))
	f.Add([]byte(`{"series":[{"metric":"a","type":"count","interval":10,"tags":[],"points":[[100,1e400]]}]}`))
	f.Add([]byte(`{"series":[]}`))
	f.Add([]byte(`[]`))
	now := time.Unix(200, 0)
	f.Fuzz(func(t *testing.T, body []byte) {
		valid, _, err := DecodeSeries(body, DecodeOptions{Now: now})
		if err != nil {
			return
		}
		for i := range valid {
			if err := ValidateSeries(&valid[i], DecodeOptions{Now: now}); err != nil {
				t.Fatalf("accepted series fails validation: %v", err)
			}
			if _, err := json.Marshal(valid[i]); err != nil {
				t.Fatalf("accepted series does not re-encode: %v", err)
			}
		}
	})
}
