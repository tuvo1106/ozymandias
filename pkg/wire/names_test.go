package wire

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestValidMetricName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"http.request.count", true},
		{"A_b.c9", true},
		{"a", true},
		{strings.Repeat("a", 200), true},
		{strings.Repeat("a", 201), false},
		{"", false},
		{"1abc", false},
		{"_abc", false},
		{"a-b", false},
		{"a b", false},
		{"a:b", false},
		{"é", false},
	} {
		if got := ValidMetricName(tc.name); got != tc.want {
			t.Errorf("ValidMetricName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeMetricName_RepairsCharactersRejectsTheRest(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"http.request.count", "http.request.count", true},
		{"bad-name here", "bad_name_here", true},
		{"a:b|c", "a_b_c", true},
		{"héllo", "h__llo", true}, // é is two bytes, each replaced
		{"1abc", "", false},
		{"", "", false},
		{"-abc", "", false},
		{strings.Repeat("a", 201), "", false},
	} {
		got, ok := NormalizeMetricName(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeMetricName(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidTag(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"env:dev", true},
		{"canary", true},
		{"route:/api/comics/:id", true},
		{"url:http://x:8080", true},
		{"env:DEV", true}, // values may be upper case; only keys are constrained
		{"k.a-b/c_d:v", true},
		{"k:" + strings.Repeat("v", 198), true}, // exactly 200 bytes
		{"k:" + strings.Repeat("v", 199), false},
		{strings.Repeat("k", 101), false},
		{"", false},
		{"Env:dev", false},
		{"1env:dev", false},
		{"en v:dev", false},
		{"env:a,b", false},
		{"env:a\nb", false},
		{"env:\xff", false},
	} {
		if got := ValidTag(tc.tag); got != tc.want {
			t.Errorf("ValidTag(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}

func TestNormalizeTag(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"env:dev", "env:dev", true},
		{"Env:DEV", "env:dev", true},
		{"canary", "canary", true},
		{"canary:", "canary", true},
		{"my key:v", "my_key:v", true},
		{"a@b", "a_b", true},
		{"a@b:Ω", "a_b:ω", true},
		{"route:/api/comics", "route:/api/comics", true},
		{"1env:dev", "", false},
		{":dev", "", false},
		{"", "", false},
		{"\xff", "", false},
		{"k:" + strings.Repeat("v", 199), "", false},
		{strings.Repeat("k", 101) + ":v", "", false},
		{"env:a,b", "", false},
	} {
		got, ok := NormalizeTag(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSplitJoinTag(t *testing.T) {
	for _, tc := range []struct{ tag, k, v string }{
		{"env:dev", "env", "dev"},
		{"canary", "canary", ""},
		{"url:http://x:1", "url", "http://x:1"},
	} {
		k, v := SplitTag(tc.tag)
		if k != tc.k || v != tc.v {
			t.Errorf("SplitTag(%q) = %q, %q", tc.tag, k, v)
		}
		if got := JoinTag(k, v); got != tc.tag {
			t.Errorf("JoinTag(%q, %q) = %q, want %q", k, v, got, tc.tag)
		}
	}
}

func TestCanonicalTags_SortsAndDeduplicates(t *testing.T) {
	got := CanonicalTags([]string{"b:1", "a", "b:1", "a:2", "a"})
	want := []string{"a", "a:2", "b:1"}
	if !slices.Equal(got, want) {
		t.Fatalf("CanonicalTags = %q, want %q", got, want)
	}
	if !HasTagKey(got, "b") || HasTagKey(got, "c") || !HasTagKey(got, "a") {
		t.Fatal("HasTagKey wrong")
	}
}

// L2: whatever NormalizeTag and NormalizeMetricName return is valid and
// already normal — normalizing twice changes nothing.
func TestNormalize_OutputIsValidAndIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		if tag, ok := NormalizeTag(s); ok {
			if !ValidTag(tag) {
				t.Fatalf("NormalizeTag(%q) = %q, which is invalid", s, tag)
			}
			if again, ok2 := NormalizeTag(tag); !ok2 || again != tag {
				t.Fatalf("NormalizeTag not idempotent: %q → %q → %q", s, tag, again)
			}
		}
		if name, ok := NormalizeMetricName(s); ok {
			if !ValidMetricName(name) {
				t.Fatalf("NormalizeMetricName(%q) = %q, which is invalid", s, name)
			}
			if again, _ := NormalizeMetricName(name); again != name {
				t.Fatalf("NormalizeMetricName not idempotent: %q → %q", name, again)
			}
		}
	})
}

// L2: tag order never changes identity.
func TestCanonicalTags_OrderIndependent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tags := rapid.SliceOf(rapid.SampledFrom([]string{"a", "a:1", "b:2", "c", "env:dev", "z:9"})).Draw(t, "tags")
		shuffled := slices.Clone(tags)
		rapid.Permutation(shuffled).Draw(t, "perm")
		if a, b := CanonicalTags(slices.Clone(tags)), CanonicalTags(shuffled); !slices.Equal(a, b) {
			t.Fatalf("%q and %q canonicalize differently: %q vs %q", tags, shuffled, a, b)
		}
	})
}
