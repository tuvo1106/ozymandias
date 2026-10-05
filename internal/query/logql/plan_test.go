package logql

import (
	"fmt"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func TestSplit(t *testing.T) {
	for _, c := range []struct {
		q       string
		matcher []Matcher
		filter  string // canonical text of the branch filter; "" = none
		arms    int
	}{
		{"", nil, "", 1},
		{"service:api", []Matcher{{Key: "service", Value: "api"}}, "", 1},
		{"service:api status:error timeout", []Matcher{{Key: "service", Value: "api"}, {Key: "status", Value: "error"}}, "timeout", 1},
		{"-status:debug service:a*", []Matcher{{Key: "status", Value: "debug", Negate: true}, {Key: "service", Value: "a*"}}, "", 1},
		{"service:api @ms:>200", []Matcher{{Key: "service", Value: "api"}}, "@ms:>200", 1},
		// trace_id is a field, not a stream label: a scan.
		{"trace_id:abc service:api", []Matcher{{Key: "service", Value: "api"}}, "trace_id:abc", 1},
		// Negating a field that is not a stream label is a scan, not an index lookup.
		{"service:api -trace_id:abc", []Matcher{{Key: "service", Value: "api"}}, "-trace_id:abc", 1},
		// A parenthesized AND flattens into the conjunction.
		{"(service:api status:error) timeout", []Matcher{{Key: "service", Value: "api"}, {Key: "status", Value: "error"}}, "timeout", 1},
		// Negating a non-label stays in the filter.
		{"service:api -timeout", []Matcher{{Key: "service", Value: "api"}}, "-timeout", 1},
		{"-(service:api)", []Matcher{{Key: "service", Value: "api", Negate: true}}, "", 1},
		{"-(service:api OR service:web)", nil, "-(service:api OR service:web)", 1},
		// A top-level OR is a union of branches.
		{"service:api OR service:worker", []Matcher{{Key: "service", Value: "api"}}, "", 2},
		{"service:api timeout OR service:worker status:error", []Matcher{{Key: "service", Value: "api"}}, "timeout", 2},
		// A nested OR is not distributed: it is scanned.
		{"service:api (status:error OR status:critical)", []Matcher{{Key: "service", Value: "api"}}, "status:error OR status:critical", 1},
		{"(a OR b) OR c", nil, "a", 3},
	} {
		n, err := Parse(c.q)
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		p := Split(n)
		if len(p.Branches) != c.arms {
			t.Errorf("%q: %d branches, want %d", c.q, len(p.Branches), c.arms)
			continue
		}
		b := p.Branches[0]
		if !reflect.DeepEqual(b.Matchers, c.matcher) {
			t.Errorf("%q: matchers %+v, want %+v", c.q, b.Matchers, c.matcher)
		}
		got := ""
		if b.Filter != nil {
			got = b.Filter.String()
		}
		if got != c.filter {
			t.Errorf("%q: filter %q, want %q", c.q, got, c.filter)
		}
	}
}

// The property the split exists to keep: running the plan (streams that pass
// the matchers, then the filter on their logs) finds exactly the logs the
// whole query finds. If lifting a conjunct into the index ever changed an
// answer, a search would silently miss or invent logs.
func TestProperty_PlanAgreesWithTheWholeQuery(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := vocabQuery(0).Draw(t, "query")
		whole := Compile(q)
		plan := Split(q)
		filters := make([]Filter, len(plan.Branches))
		for i, b := range plan.Branches {
			filters[i] = Compile(b.Filter)
		}
		for i := 0; i < 20; i++ {
			l := vocabLog().Draw(t, fmt.Sprintf("log%d", i))
			want := whole(l)
			got := false
			for bi, b := range plan.Branches {
				ok := true
				for _, m := range b.Matchers {
					if !m.Matches(LabelValue(l, m.Key)) {
						ok = false
						break
					}
				}
				if ok && filters[bi](l) {
					got = true
					break
				}
			}
			if got != want {
				t.Fatalf("query %q on %+v: whole=%v plan=%v\nplan: %+v", q, l, want, got, plan)
			}
		}
	})
}

var (
	vocabServices = []string{"api", "worker", "web"}
	vocabStatuses = []string{"debug", "info", "warn", "error"}
	vocabHosts    = []string{"", "h1", "h2"}
	vocabEnvs     = []string{"", "dev", "prod"}
	vocabWords    = []string{"timeout", "refused", "ok", "slow"}
)

func vocabLog() *rapid.Generator[*wire.Log] {
	return rapid.Custom(func(t *rapid.T) *wire.Log {
		l := &wire.Log{
			Ts:      1,
			Service: rapid.SampledFrom(vocabServices).Draw(t, "svc"),
			Status:  rapid.SampledFrom(vocabStatuses).Draw(t, "status"),
			Host:    rapid.SampledFrom(vocabHosts).Draw(t, "host"),
			Message: rapid.SampledFrom(vocabWords).Draw(t, "w1") + " " + rapid.SampledFrom(vocabWords).Draw(t, "w2"),
			Attrs:   map[string]any{"ms": float64(rapid.IntRange(0, 4).Draw(t, "ms") * 100)},
		}
		if env := rapid.SampledFrom(vocabEnvs).Draw(t, "env"); env != "" {
			l.Tags = []string{"env:" + env}
		}
		if rapid.Bool().Draw(t, "trace") {
			l.TraceID = "0123456789abcdef0123456789abcdef"
		}
		return l
	})
}

// vocabQuery draws queries over the same vocabulary the logs use, so a good
// share of them match something.
func vocabQuery(depth int) *rapid.Generator[Node] {
	leaf := rapid.Custom(func(t *rapid.T) Node {
		switch rapid.IntRange(0, 6).Draw(t, "leaf") {
		case 0:
			return Label{"service", rapid.SampledFrom([]string{"api", "worker", "a*", "*", "nope"}).Draw(t, "v")}
		case 1:
			return Label{"status", rapid.SampledFrom(vocabStatuses).Draw(t, "v")}
		case 2:
			return Label{"host", rapid.SampledFrom([]string{"h1", "h*", "*", "h3"}).Draw(t, "v")}
		case 3:
			return Label{"env", rapid.SampledFrom([]string{"dev", "prod", "*", "d*"}).Draw(t, "v")}
		case 4:
			return Label{"trace_id", rapid.SampledFrom([]string{"0123*", "*", "zzz"}).Draw(t, "v")}
		case 5:
			return Text{rapid.SampledFrom(append([]string{"*", "tim*out"}, vocabWords...)).Draw(t, "v")}
		}
		return Attr{Path: "ms", Op: Op(rapid.IntRange(1, 4).Draw(t, "op")), Num: float64(rapid.IntRange(0, 4).Draw(t, "n") * 100)}
	})
	if depth >= 3 {
		return leaf
	}
	return rapid.Custom(func(t *rapid.T) Node {
		switch rapid.IntRange(0, 5).Draw(t, "kind") {
		case 0:
			return Not{X: vocabQuery(depth+1).Draw(t, "not")}
		case 1:
			return Or{Args: rapid.SliceOfN(vocabQuery(depth+1), 2, 3).Draw(t, "or")}
		case 2:
			return And{Args: rapid.SliceOfN(vocabQuery(depth+1), 2, 3).Draw(t, "and")}
		}
		return leaf.Draw(t, "leaf")
	})
}
