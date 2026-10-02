package agenttags

import (
	"fmt"
	"slices"
	"testing"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func TestDecorate(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tags, agentTags []string
		want            []string
	}{
		{"host added", []string{"k:v"}, []string{"env:dev"}, []string{"env:dev", "host:mac", "k:v"}},
		{"the series names its host", []string{"host:db1"}, []string{"env:dev"}, []string{"env:dev", "host:db1"}},
		{"a global tag names the host", nil, []string{"host:db1"}, []string{"host:db1"}},
		{"duplicates collapse", []string{"env:dev", "env:dev"}, []string{"env:dev"}, []string{"env:dev", "host:mac"}},
	} {
		got, dropped := Decorate(tc.tags, tc.agentTags, "host:mac")
		if !slices.Equal(got, tc.want) || dropped != 0 {
			t.Errorf("%s: %v (%d dropped), want %v", tc.name, got, dropped, tc.want)
		}
	}
}

func TestDecorate_Cap(t *testing.T) {
	var many []string
	for i := range wire.MaxTagsPerPoint + 5 {
		many = append(many, fmt.Sprintf("k%03d:v", i))
	}
	got, dropped := Decorate(many, nil, "host:mac")
	if len(got) != wire.MaxTagsPerPoint || dropped != 6 {
		t.Fatalf("len %d, dropped %d", len(got), dropped)
	}
	rev := slices.Clone(many)
	slices.Reverse(rev)
	again, _ := Decorate(rev, nil, "host:mac")
	if !slices.Equal(got, again) {
		t.Fatal("the cap kept a different subset for a different order")
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize([]string{"Env:Dev", "a:b,c", " "}); !slices.Equal(got, []string{"env:dev"}) {
		t.Fatalf("got %v", got)
	}
}

// Review finding: the cap cut the sorted set, so a replica tag behind fifty
// scraped labels was dropped and two replicas wrote one series. Keep tags,
// the agent's tags, the host tag and an own host tag survive; own tags
// give way, the same ones whatever their order.
func TestDecorateKeeping_TheCapTrimsOwnTagsFirst(t *testing.T) {
	var own []string
	for i := range wire.MaxTagsPerPoint + 5 {
		own = append(own, fmt.Sprintf("k%03d:v", i))
	}
	keep := []string{"replica:1", "zz_instance:cache", "k000:v"} // k000 is both
	got, dropped := DecorateKeeping(own, keep, []string{"env:dev"}, "host:mac")
	if len(got) != wire.MaxTagsPerPoint {
		t.Fatalf("len %d", len(got))
	}
	for _, want := range []string{"replica:1", "zz_instance:cache", "k000:v", "env:dev", "host:mac"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s was cut: %v", want, got)
		}
	}
	// 55 own (one also kept) + 2 kept + env + host = 59 distinct; 50 fit.
	if dropped != 9 {
		t.Errorf("dropped %d, want 9", dropped)
	}
	rev := slices.Clone(own)
	slices.Reverse(rev)
	if again, _ := DecorateKeeping(rev, keep, []string{"env:dev"}, "host:mac"); !slices.Equal(got, again) {
		t.Fatal("the cap kept a different subset for a different order")
	}
	// A check that reports another machine's host keeps it.
	got, _ = DecorateKeeping(append(own, "host:db1"), keep, nil, "host:mac")
	if !slices.Contains(got, "host:db1") || slices.Contains(got, "host:mac") {
		t.Fatalf("own host tag: %v", got)
	}
	// Nothing to keep: exactly Decorate.
	a, da := DecorateKeeping(slices.Clone(own), nil, nil, "host:mac")
	b, db := Decorate(slices.Clone(own), nil, "host:mac")
	if !slices.Equal(a, b) || da != db {
		t.Fatal("with no keep tags it is not Decorate")
	}
}

// Below the cap the fast path gives what the full path would: every tag,
// canonical, the host added unless one is there.
func TestDecorateKeeping_UnderTheCap(t *testing.T) {
	got, dropped := DecorateKeeping([]string{"b:1", "a:1"}, []string{"replica:0", "a:1"}, []string{"env:dev"}, "host:mac")
	if want := []string{"a:1", "b:1", "env:dev", "host:mac", "replica:0"}; !slices.Equal(got, want) || dropped != 0 {
		t.Fatalf("got %v (%d dropped), want %v", got, dropped, want)
	}
	got, _ = DecorateKeeping([]string{"host:db1"}, []string{"replica:0"}, nil, "host:mac")
	if !slices.Equal(got, []string{"host:db1", "replica:0"}) {
		t.Fatalf("own host: %v", got)
	}
}

// Review finding: a scraped service="checkout" beside the container's
// service:shop-api gave one series two service values, counted in both
// groups of a group-by. The scraped one is renamed exported_service, as
// Prometheus does; host is left alone (a check may report another machine).
func TestExportClashes_RenamesClashingOwnTags(t *testing.T) {
	got, _ := decorateCheck([]string{"service:checkout", "route:/x", "host:db1"}, []string{"service:shop-api", "host:other"}, nil, "host:mac")
	for _, want := range []string{"exported_service:checkout", "service:shop-api", "route:/x", "host:db1"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing: %v", want, got)
		}
	}
	if slices.Contains(got, "service:checkout") {
		t.Errorf("the scraped service kept its key: %v", got)
	}
	// Past the cap too.
	own := []string{"service:checkout"}
	for i := range wire.MaxTagsPerPoint {
		own = append(own, fmt.Sprintf("k%03d:v", i))
	}
	got, _ = decorateCheck(own, []string{"service:shop-api"}, nil, "")
	if n := len(slices.DeleteFunc(slices.Clone(got), func(t string) bool { k, _ := wire.SplitTag(t); return k != "service" })); n != 1 {
		t.Fatalf("%d service tags past the cap: %v", n, got)
	}
}

// Review finding: a scraped env="staging" beside the agent's own env:prod
// kept both. Agent tags clash like keep tags.
func TestExportClashes_AgentTagsClashToo(t *testing.T) {
	got, _ := decorateCheck([]string{"env:staging", "route:/x"}, []string{"replica:0"}, []string{"env:prod"}, "host:mac")
	if !slices.Contains(got, "exported_env:staging") || !slices.Contains(got, "env:prod") || slices.Contains(got, "env:staging") {
		t.Fatalf("%v", got)
	}
}

// decorateCheck is what the scheduler does with a check's metric.
func decorateCheck(own, keep, agentTags []string, hostTag string) ([]string, int) {
	own, renamed := ExportClashes(slices.Clone(own), keep, agentTags)
	out, dropped := DecorateKeeping(own, keep, agentTags, hostTag)
	return out, dropped + renamed
}

// DecorateKeeping alone renames nothing: a built-in's service:shop-api
// beside an agent-wide service:infra keeps both, as Decorate does.
func TestDecorateKeeping_RenamesNothing(t *testing.T) {
	got, _ := DecorateKeeping([]string{"service:shop-api"}, []string{"replica:0"}, []string{"service:infra"}, "host:mac")
	if !slices.Contains(got, "service:shop-api") || !slices.Contains(got, "service:infra") {
		t.Fatalf("%v", got)
	}
}
