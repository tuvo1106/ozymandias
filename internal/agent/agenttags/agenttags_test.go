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
