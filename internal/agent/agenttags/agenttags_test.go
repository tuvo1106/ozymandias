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
