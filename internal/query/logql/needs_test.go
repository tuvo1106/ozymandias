package logql

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func TestNeeds(t *testing.T) {
	for q, want := range map[string]string{
		"":                       "none",
		"timeout":                "[[timeout]]",
		"TimeOut":                "[[timeout]]",
		"ab":                     "none", // too short to look up
		"tim*out":                "[[tim out]]",
		"*":                      "none",
		"a*b*cde":                "[[cde]]",
		"-timeout":               "none", // absence is not provable
		"service:api":            "none",
		"@ms:>5":                 "none",
		"@ms:[1 TO 5]":           "none",
		"@ms:1000":               "none", // numbers compare as numbers: 1e3 would match
		"@ms:*":                  "none",
		"@user:bob":              "[[bob]]",
		"@user:Zoë":              "[[zoë]]",
		"timeout refused":        "[[timeout refused]]",
		"timeout -refused":       "[[timeout]]",
		"timeout OR refused":     "[[timeout] [refused]]",
		"timeout OR ab":          "none", // one arm says nothing, so the whole OR does not
		"(a1x OR b2y) timeout":   "[[a1x timeout] [b2y timeout]]",
		"service:api timeout":    "[[timeout]]",
		"timeout OR service:api": "none",
	} {
		n, err := Parse(q)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		got := Needs(n)
		gs := "none"
		if !got.None() {
			gs = fmt.Sprint(got.Alts)
		}
		if gs != want {
			t.Errorf("Needs(%q) = %s, want %s", q, gs, want)
		}
	}
	if !OrNeeds([]Need{{Alts: [][]string{{"abc"}}}, {}}).None() {
		t.Error("an unconstrained branch must make the union unconstrained")
	}
	if OrNeeds([]Need{{Alts: [][]string{{"abc"}}}, {Alts: [][]string{{"def"}}}}).None() {
		t.Error("two constrained branches should stay constrained")
	}
	var many []Need
	for i := 0; i < 17; i++ {
		many = append(many, Need{Alts: [][]string{{fmt.Sprintf("lit%d", i)}}})
	}
	if !OrNeeds(many).None() {
		t.Error("more alternatives than the cap must fall back to unconstrained")
	}
}

func TestNeeds_AreCappedNotExplosive(t *testing.T) {
	var parts []string
	for i := 0; i < 10; i++ {
		parts = append(parts, fmt.Sprintf("(aa%dx OR bb%dy)", i, i))
	}
	n, err := Parse(strings.Join(parts, " "))
	if err != nil {
		t.Fatal(err)
	}
	if got := Needs(n); len(got.Alts) > maxAlts {
		t.Fatalf("%d alternatives: ten ORs ANDed together must not multiply out to 1024", len(got.Alts))
	}
	var arms []string
	for i := 0; i < 20; i++ {
		arms = append(arms, fmt.Sprintf("word%d", i))
	}
	n, _ = Parse(strings.Join(arms, " OR "))
	if !Needs(n).None() {
		t.Fatal("an OR of more arms than the cap should be unconstrained")
	}
}

// windowsOf is an exact stand-in for the store's bloom filter: the set of
// lowercased three-byte windows of a block of logs' text. Soundness of Needs
// does not depend on the filter being approximate, only on what it is a filter
// OF, so an exact set tests it without false positives hiding a bug.
func windowsOf(logs []*wire.Log) map[string]bool {
	set := map[string]bool{}
	add := func(s string) {
		s = strings.ToLower(s)
		for i := 0; i+2 < len(s); i++ {
			set[s[i:i+3]] = true
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			add(v)
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		case nil:
		default:
			add(ValueText(v))
		}
	}
	for _, l := range logs {
		add(l.Message)
		walk(l.Attrs)
	}
	return set
}

func satisfied(n Need, set map[string]bool) bool {
	if n.None() {
		return true
	}
	for _, alt := range n.Alts {
		ok := true
		for _, lit := range alt {
			for i := 0; i+2 < len(lit); i++ {
				if !set[lit[i:i+3]] {
					ok = false
				}
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// A log that matches a query is always in a block whose text satisfies the
// query's Need: the property that makes skipping on a filter safe.
func TestNeeds_AreSoundAgainstTheMatcher(t *testing.T) {
	words := []string{"timeout", "Refused", "épée", "ÉPÉE", "alpha-beta", "x", "ab", "gamma_delta", "Zoë", "12345", "1e3", "true", "/api/v1/items", "a.b.c"}
	queries := []string{
		"timeout", "TIMEOUT", "refus*", "*fused", "t*e*t", "épée", "ÉPÉE", "alpha-b*", "gamma_d*a", "zoë", "123", "12345", "1e3", "1000",
		"/api/v1", "a.b", "ab", "x", "*", "**", "true", "-timeout", "timeout OR épée", "timeout refused", "(timeout OR zoë) refus*",
		"@user:zoë", "@user:Z*", "@user:*", "@n:12345", "@n:1000", "@n:>5", "@n:1e3", "@tags:épée", "@o.p:alpha-beta", "@ok:true",
		"(timeout OR ab) refused", "-(timeout OR refused) x", "service:api timeout", "timeout OR service:api",
	}
	rapid.Check(t, func(t *rapid.T) {
		pick := func(label string) string { return rapid.SampledFrom(words).Draw(t, label) }
		var logs []*wire.Log
		for i := 0; i < rapid.IntRange(1, 6).Draw(t, "n"); i++ {
			logs = append(logs, logFrom(t, pick(fmt.Sprint("a", i))+" "+pick(fmt.Sprint("b", i)), "info",
				fmt.Sprintf(`{"user":%q,"n":%s,"tags":[%q],"o":{"p":%q},"ok":%t}`,
					pick(fmt.Sprint("u", i)), rapid.SampledFrom([]string{"12345", "1e3", "1000", "7"}).Draw(t, fmt.Sprint("nn", i)),
					pick(fmt.Sprint("t", i)), pick(fmt.Sprint("op", i)), rapid.Bool().Draw(t, fmt.Sprint("ok", i)))))
		}
		set := windowsOf(logs)
		for _, q := range queries {
			n, err := Parse(q)
			if err != nil {
				t.Fatalf("%q: %v", q, err)
			}
			f, need := Compile(n), Needs(n)
			for _, l := range logs {
				if f(l) && !satisfied(need, set) {
					t.Fatalf("query %q matches %q %v but the block's text does not satisfy its need %v", q, l.Message, l.Attrs, need.Alts)
				}
			}
		}
	})
}

// The test above could pass if Needs said "none" for everything. This is the
// other half: it must say something useful for the queries that have a literal.
func TestNeeds_AreNotVacuous(t *testing.T) {
	set := windowsOf([]*wire.Log{logFrom(t, "all quiet", "info", `{"user":"bob"}`)})
	for _, q := range []string{"timeout", "refus*", "@user:alice", "timeout OR refused", "quiet timeout"} {
		n, _ := Parse(q)
		if satisfied(Needs(n), set) {
			t.Errorf("%q should have been ruled out by a block that has none of its words", q)
		}
	}
	n, _ := Parse("quiet")
	if !satisfied(Needs(n), set) {
		t.Error("a word that is in the block was ruled out")
	}
}
