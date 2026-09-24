package index

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// build indexes the given series, returning the index and the ids in order.
func build(t *testing.T, refs ...tsdb.SeriesRef) (*MemPostings, []uint64) {
	t.Helper()
	p := NewMemPostings()
	ids := make([]uint64, len(refs))
	for i, r := range refs {
		ids[i] = uint64(i + 1)
		p.Add(ids[i], r)
	}
	return p, ids
}

func ref(metric string, tags ...string) tsdb.SeriesRef {
	return tsdb.NewSeriesRef(metric, tags)
}

func sel(metric string, ms ...tsdb.Matcher) tsdb.Selector {
	return tsdb.Selector{Metric: metric, Matchers: ms}
}

func eq(k, v string) tsdb.Matcher   { return tsdb.Matcher{Key: k, Value: v, Type: tsdb.Equal} }
func ne(k, v string) tsdb.Matcher   { return tsdb.Matcher{Key: k, Value: v, Type: tsdb.NotEqual} }
func glob(k, v string) tsdb.Matcher { return tsdb.Matcher{Key: k, Value: v, Type: tsdb.Wildcard} }
func nglob(k, v string) tsdb.Matcher {
	return tsdb.Matcher{Key: k, Value: v, Type: tsdb.NotWildcard}
}

func TestMemPostings_Select(t *testing.T) {
	refs := []tsdb.SeriesRef{
		ref("http.request.count", "env:prod", "service:web", "route:/api"),   // 1
		ref("http.request.count", "env:prod", "service:web", "route:/admin"), // 2
		ref("http.request.count", "env:dev", "service:web", "route:/api"),    // 3
		ref("http.request.count", "env:prod", "service:worker"),              // 4
		ref("queue.depth", "env:prod", "service:worker"),                     // 5
	}
	p, _ := build(t, refs...)

	for _, tc := range []struct {
		name string
		sel  tsdb.Selector
		want []uint64
	}{
		{"metric only", sel("http.request.count"), []uint64{1, 2, 3, 4}},
		{"metric and tag", sel("http.request.count", eq("env", "prod")), []uint64{1, 2, 4}},
		{"two tags intersect", sel("http.request.count", eq("env", "prod"), eq("service", "web")), []uint64{1, 2}},
		{"no metric matches across metrics", sel("", eq("service", "worker")), []uint64{4, 5}},
		{"unknown metric", sel("nope"), nil},
		{"unknown tag value", sel("http.request.count", eq("env", "staging")), nil},
		// A series without the key at all satisfies a not-equal matcher —
		// that is the reference semantics in tsdb.Matcher.Matches.
		{"not-equal includes series lacking the key", sel("http.request.count", ne("route", "/api")), []uint64{2, 4}},
		{"wildcard", sel("http.request.count", glob("route", "/a*")), []uint64{1, 2, 3}},
		{"wildcard matches nothing", sel("http.request.count", glob("route", "/zzz*")), nil},
		{"not-wildcard", sel("http.request.count", nglob("route", "/a*")), []uint64{4}},
		{"everything", sel(""), []uint64{1, 2, 3, 4, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Select(tc.sel)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Select = %v, want %v", got, tc.want)
			}
		})
	}
}

// The index is an optimization, and an optimization that disagrees with the
// definition is a bug. tsdb.Matcher.Matches is the definition.
func TestMemPostings_AgreesWithReferenceSemantics(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	metrics := []string{"m.one", "m.two"}
	envs := []string{"prod", "dev", "staging"}
	svcs := []string{"web", "worker", "cron"}

	var refs []tsdb.SeriesRef
	for i := 0; i < 200; i++ {
		tags := []string{"env:" + envs[rng.Intn(len(envs))]}
		if rng.Intn(4) > 0 { // some series lack the service tag entirely
			tags = append(tags, "service:"+svcs[rng.Intn(len(svcs))])
		}
		if rng.Intn(3) == 0 {
			tags = append(tags, fmt.Sprintf("shard:%d", rng.Intn(5)))
		}
		refs = append(refs, ref(metrics[rng.Intn(len(metrics))], tags...))
	}
	p, ids := build(t, refs...)

	matchers := []tsdb.Matcher{
		eq("env", "prod"), eq("service", "web"), eq("shard", "2"),
		ne("env", "prod"), ne("service", "web"), ne("shard", "2"),
		glob("env", "*d*"), glob("service", "w*"), nglob("env", "*d*"), nglob("service", "w*"),
	}
	for i, m1 := range matchers {
		for _, m2 := range matchers {
			for _, metric := range []string{"", metrics[0]} {
				s := sel(metric, m1, m2)
				got := p.Select(s)

				var want []uint64
				for j, r := range refs {
					if metric != "" && r.Metric != metric {
						continue
					}
					if m1.Matches(r.Tags) && m2.Matches(r.Tags) {
						want = append(want, ids[j])
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("case %d: metric=%q %v AND %v\n got %v\nwant %v", i, metric, m1, m2, got, want)
				}
			}
		}
	}
}

func TestMemPostings_DeleteRemovesTheSeriesEverywhere(t *testing.T) {
	p, ids := build(t,
		ref("m", "env:prod", "host:a"),
		ref("m", "env:prod", "host:b"),
	)
	p.Delete(ids[0], ref("m", "env:prod", "host:a"))

	if got := p.Select(sel("m")); !reflect.DeepEqual(got, []uint64{ids[1]}) {
		t.Errorf("after delete, Select = %v, want %v", got, []uint64{ids[1]})
	}
	if got := p.Select(sel("", eq("host", "a"))); got != nil {
		t.Errorf("deleted series still in postings: %v", got)
	}
	// The empty container must go too, or a high-cardinality tag leaks map
	// entries for every value it ever had.
	if _, ok := p.m["host"]["a"]; ok {
		t.Error("empty postings list was left behind")
	}
	if series, _ := p.Size(); series != 1 {
		t.Errorf("Size reports %d series, want 1", series)
	}
	// Deleting something absent is a no-op, not a panic.
	p.Delete(999, ref("m", "env:prod", "host:zzz"))
}

func TestMemPostings_Metadata(t *testing.T) {
	p, _ := build(t,
		ref("http.request.count", "env:prod", "route:/api"),
		ref("http.request.count", "env:dev"),
		ref("queue.depth", "worker:1"),
	)
	if got, want := p.Metrics(""), []string{"http.request.count", "queue.depth"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Metrics() = %v, want %v", got, want)
	}
	if got, want := p.Metrics("http"), []string{"http.request.count"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Metrics(http) = %v, want %v", got, want)
	}
	if got, want := p.Keys("http.request.count"), []string{"env", "route"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
	if got, want := p.Keys(""), []string{"env", "route", "worker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Keys(all) = %v, want %v", got, want)
	}
	if got, want := p.Values("http.request.count", "env"), []string{"dev", "prod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Values = %v, want %v", got, want)
	}
	// A key that exists only on another metric is not reported for this one.
	if got := p.Values("http.request.count", "worker"); got != nil {
		t.Errorf("Values for another metric's key = %v, want none", got)
	}
	if got := p.Keys("nope"); got != nil {
		t.Errorf("Keys for an unknown metric = %v", got)
	}
	if got := p.Values("", "nope"); got != nil {
		t.Errorf("Values for an unknown key = %v", got)
	}
}

func TestMemPostings_AddIsIdempotentAndOrderIndependent(t *testing.T) {
	// WAL replay can re-add a series that is already indexed, and ids do not
	// always arrive in order there.
	p := NewMemPostings()
	r := ref("m", "env:prod")
	p.Add(5, r)
	p.Add(3, ref("m", "env:prod"))
	p.Add(5, r) // duplicate
	got := p.Select(sel("m"))
	if want := []uint64{3, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("Select = %v, want %v (sorted, de-duplicated)", got, want)
	}
}

func TestMemPostings_SelectResultIsACopy(t *testing.T) {
	// Handing out the index's own slice would let a caller corrupt it.
	p, _ := build(t, ref("m", "env:prod"), ref("m", "env:prod"))
	got := p.Select(sel("m"))
	got[0] = 999
	if again := p.Select(sel("m")); again[0] == 999 {
		t.Error("Select returned a slice aliasing the index")
	}
}

func TestIntersect_GallopsCorrectly(t *testing.T) {
	// The galloping search is the easiest thing here to get subtly wrong, so
	// check it against a naive intersection on adversarial shapes.
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 200; trial++ {
		a := randomIDs(rng, rng.Intn(50))
		b := randomIDs(rng, rng.Intn(300))
		want := naiveIntersect(a, b)
		if got := intersect(a, b); !equalIDs(got, want) {
			t.Fatalf("intersect(%v, %v) = %v, want %v", a, b, got, want)
		}
	}
}

func TestSubtractAndMerge(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	for trial := 0; trial < 100; trial++ {
		a := randomIDs(rng, rng.Intn(40))
		b := randomIDs(rng, rng.Intn(40))
		var want []uint64
		for _, id := range a {
			if !contains(b, id) {
				want = append(want, id)
			}
		}
		if got := subtract(a, b); !equalIDs(got, want) {
			t.Fatalf("subtract(%v, %v) = %v, want %v", a, b, got, want)
		}
		union := merge([][]uint64{a, b})
		seen := map[uint64]bool{}
		for _, id := range append(append([]uint64{}, a...), b...) {
			seen[id] = true
		}
		if len(union) != len(seen) {
			t.Fatalf("merge(%v, %v) = %v, want %d distinct ids", a, b, union, len(seen))
		}
		if !sort.SliceIsSorted(union, func(i, j int) bool { return union[i] < union[j] }) {
			t.Fatalf("merge produced an unsorted list: %v", union)
		}
	}
	if got := merge(nil); got != nil {
		t.Errorf("merge(nil) = %v", got)
	}
}

func randomIDs(rng *rand.Rand, n int) []uint64 {
	set := map[uint64]bool{}
	for i := 0; i < n; i++ {
		set[uint64(rng.Intn(500))] = true
	}
	out := make([]uint64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func naiveIntersect(a, b []uint64) []uint64 {
	var out []uint64
	for _, id := range a {
		if contains(b, id) {
			out = append(out, id)
		}
	}
	return out
}

func contains(list []uint64, id uint64) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
