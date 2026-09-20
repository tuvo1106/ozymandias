package tsdb

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestTag_StringAndParse(t *testing.T) {
	for _, s := range []string{"env:dev", "canary", "url:http://x:1"} {
		if got := ParseTag(s).String(); got != s {
			t.Errorf("round trip %q → %q", s, got)
		}
	}
	if (Tag{Key: "k"}).String() != "k" {
		t.Error("bare tag")
	}
}

func TestNewSeriesRef_IsCanonical(t *testing.T) {
	ref := NewSeriesRef("m", []string{"b:2", "a", "b:1", "a"})
	if got := ref.Key(); got != "m|a,b:1,b:2" {
		t.Fatalf("Key = %q", got)
	}
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	if v, ok := ref.Get("b"); !ok || v != "1" {
		t.Errorf("Get(b) = %q, %v", v, ok)
	}
	if _, ok := ref.Get("zz"); ok {
		t.Error("Get(zz) found")
	}
}

func TestSeriesRef_ValidateRejectsNonCanonical(t *testing.T) {
	for name, ref := range map[string]SeriesRef{
		"no metric": {Tags: nil},
		"unsorted":  {Metric: "m", Tags: []Tag{{"b", ""}, {"a", ""}}},
		"duplicate": {Metric: "m", Tags: []Tag{{"a", "1"}, {"a", "1"}}},
		"values":    {Metric: "m", Tags: []Tag{{"a", "2"}, {"a", "1"}}},
	} {
		if ref.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatcher_Semantics(t *testing.T) {
	tags := NewSeriesRef("m", []string{"env:dev", "route:/api/comics", "canary"}).Tags
	for _, tc := range []struct {
		m    Matcher
		want bool
	}{
		{Matcher{Key: "env", Value: "dev", Type: Equal}, true},
		{Matcher{Key: "env", Value: "prod", Type: Equal}, false},
		{Matcher{Key: "canary", Value: "", Type: Equal}, true},
		{Matcher{Key: "zone", Value: "a", Type: Equal}, false},
		{Matcher{Key: "env", Value: "prod", Type: NotEqual}, true},
		{Matcher{Key: "env", Value: "dev", Type: NotEqual}, false},
		{Matcher{Key: "zone", Value: "a", Type: NotEqual}, true}, // no key: not equal
		{Matcher{Key: "route", Value: "/api/*", Type: Wildcard}, true},
		{Matcher{Key: "route", Value: "*comics", Type: Wildcard}, true},
		{Matcher{Key: "route", Value: "/web/*", Type: Wildcard}, false},
		{Matcher{Key: "zone", Value: "*", Type: Wildcard}, false}, // key must exist
		{Matcher{Key: "route", Value: "/api/*", Type: NotWildcard}, false},
		{Matcher{Key: "zone", Value: "*", Type: NotWildcard}, true},
	} {
		if got := tc.m.Matches(tags); got != tc.want {
			t.Errorf("%+v.Matches = %v, want %v", tc.m, got, tc.want)
		}
	}
}

func TestGlobMatch_OnlyStarIsSpecial(t *testing.T) {
	for _, tc := range []struct {
		pat, s string
		want   bool
	}{
		{"a*", "abc", true},
		{"*", "", true},
		{"a?c", "abc", false},
		{"a?c", "a?c", true},
		{"[ab]", "a", false},
		{"[ab]", "[ab]", true},
		{`a\b`, `a\b`, true},
		{"/api/*/x", "/api/1/2/x", true}, // '*' crosses '/'
		{"*.png", "cover.png", true},
	} {
		if got := GlobMatch(tc.pat, tc.s); got != tc.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", tc.pat, tc.s, got, tc.want)
		}
	}
}

// L2: a pattern with no '*' matches exactly itself.
func TestGlobMatch_LiteralPatternsAreEquality(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := rapid.StringMatching(`[^*]{0,12}`).Draw(t, "p")
		s := rapid.OneOf(rapid.Just(p), rapid.String()).Draw(t, "s")
		if GlobMatch(p, s) != (p == s) {
			t.Fatalf("GlobMatch(%q, %q) = %v", p, s, p != s)
		}
	})
}

func TestSelector_Matches(t *testing.T) {
	ref := NewSeriesRef("m", []string{"env:dev"})
	if !(Selector{Metric: "m"}).Matches(ref) {
		t.Error("no matchers should select every series of the metric")
	}
	if (Selector{Metric: "other"}).Matches(ref) {
		t.Error("other metric selected")
	}
	if (Selector{Metric: "m", Matchers: []Matcher{{Key: "env", Value: "prod"}}}).Matches(ref) {
		t.Error("failing matcher selected")
	}
}

func TestSliceSet(t *testing.T) {
	set := NewSliceSet([]SeriesSamples{{Series: NewSeriesRef("m", nil), Samples: []Sample{{1, 2}, {3, 4}}}})
	var got []string
	for set.Next() {
		it := set.Iterator()
		for it.Next() {
			got = append(got, set.Series().Key()+"@"+strings.Repeat("x", int(it.At().T)))
		}
		if it.Err() != nil {
			t.Fatal(it.Err())
		}
	}
	if set.Err() != nil || set.Close() != nil || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
