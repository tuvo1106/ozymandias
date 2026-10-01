package collector_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
)

// echo is a check written the way a new one would be, using only the public
// surface: it decodes its own settings and emits one gauge.
type echoConfig struct {
	Value float64 `yaml:"value"`
	Every string  `yaml:"every"`
}

type echo struct{ cfg echoConfig }

func newEcho(inst collector.Instance) (collector.Collector, error) {
	var cfg echoConfig
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	return &echo{cfg: cfg}, nil
}

func (e *echo) Name() string { return "ignored: the instance name wins" }
func (e *echo) Interval() time.Duration {
	d, _ := time.ParseDuration(e.cfg.Every)
	return d
}
func (e *echo) Collect(_ context.Context, emit collector.Emit) error {
	emit(collector.Metric{Name: "echo.value", Value: e.cfg.Value, Tags: []string{"from:echo"}})
	return nil
}

var registry = collector.Registry{"echo": newEcho}

func collect(t *testing.T, c collector.Collector) []collector.Metric {
	t.Helper()
	var out []collector.Metric
	if err := c.Collect(context.Background(), func(m collector.Metric) { out = append(out, m) }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRegistry_CommonSettings(t *testing.T) {
	c, err := registry.NewInstance("echo", 0, 1, map[string]any{
		"name": "one", "interval": "30s", "tags": []any{"team:web"}, "value": 7.0,
	}, []string{"container_name:x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "echo:one" || c.Interval() != 30*time.Second {
		t.Fatalf("name %q interval %v", c.Name(), c.Interval())
	}
	got := collect(t, c)
	if len(got) != 1 || got[0].Value != 7 || !slices.Equal(got[0].Tags, []string{"from:echo"}) || !slices.Equal(got[0].Keep, []string{"team:web", "container_name:x"}) {
		t.Fatalf("emitted %+v", got)
	}
}

func TestRegistry_DefaultsComeFromTheCheck(t *testing.T) {
	c, err := registry.NewInstance("echo", 0, 1, map[string]any{"every": "20s"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "echo" || c.Interval() != 20*time.Second {
		t.Fatalf("name %q interval %v", c.Name(), c.Interval())
	}
	c, err = registry.NewInstance("echo", 2, 3, nil, nil, nil, nil)
	if err != nil || c.Name() != "echo:2" {
		t.Fatalf("an unnamed instance among three: %v, %v", c, err)
	}
}

func TestRegistry_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		check string
		raw   map[string]any
		want  string
	}{
		"unknown check":     {"nope", nil, `no such check (have echo)`},
		"misspelt setting":  {"echo", map[string]any{"valeu": 1}, "valeu"},
		"fractional period": {"echo", map[string]any{"interval": "1500ms"}, "whole number of seconds"},
		"interval not text": {"echo", map[string]any{"interval": 30}, "whole number of seconds"},
		"empty name":        {"echo", map[string]any{"name": ""}, "non-empty string"},
		"tags not a list":   {"echo", map[string]any{"tags": "a:b"}, "list"},
		"tag with a comma":  {"echo", map[string]any{"tags": []any{"a:b,c"}}, "cannot be sent"},
		"tag not text":      {"echo", map[string]any{"tags": []any{3}}, "not a string"},
	} {
		_, err := registry.NewInstance(tc.check, 0, 1, tc.raw, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestRegistry_Configured(t *testing.T) {
	cs, err := registry.Configured(map[string][]map[string]any{
		"echo": {{"name": "a"}, {"name": "b"}},
	}, nil, nil)
	if err != nil || len(cs) != 2 || cs[0].Name() != "echo:a" || cs[1].Name() != "echo:b" {
		t.Fatalf("%v, %v", cs, err)
	}
	// Every error at once, and a name used twice is one of them.
	_, err = registry.Configured(map[string][]map[string]any{
		"echo": {{"name": "a"}, {"name": "a"}},
		"nope": {{}},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "share this name") || !strings.Contains(err.Error(), "no such check") {
		t.Fatalf("err = %v", err)
	}
}

// tagsOf is the one metric an echo instance emits, by its tags.
func tagsOf(t *testing.T, c collector.Collector) []string {
	t.Helper()
	ms := collect(t, c)
	if len(ms) != 1 {
		t.Fatalf("%s emitted %d metrics", c.Name(), len(ms))
	}
	return append(ms[0].Tags, ms[0].Keep...)
}

// Two configured instances of one check must not write the same series:
// most checks say nothing about their target in their tags, so the
// instance's name (or position) is what tells them apart. A lone unnamed
// instance has nothing to collide with and gets no tag.
func TestRegistry_ConfiguredInstancesAreTagged(t *testing.T) {
	cs, err := registry.Configured(map[string][]map[string]any{
		"echo": {{"name": "Cache"}, {"name": "queue"}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := tagsOf(t, cs[0]), tagsOf(t, cs[1])
	if !slices.Contains(a, "instance:cache") || !slices.Contains(b, "instance:queue") {
		t.Fatalf("named instances tagged %v and %v", a, b)
	}
	cs, err = registry.Configured(map[string][]map[string]any{"echo": {{}, {}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := tagsOf(t, cs[0]), tagsOf(t, cs[1]); !slices.Contains(a, "instance:0") || !slices.Contains(b, "instance:1") {
		t.Fatalf("unnamed instances tagged %v and %v", a, b)
	}
	cs, err = registry.Configured(map[string][]map[string]any{"echo": {{}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, cs[0]); !slices.Equal(got, []string{"from:echo"}) {
		t.Fatalf("a lone unnamed instance tagged %v", got)
	}
	// An instance tag of the user's own wins.
	cs, err = registry.Configured(map[string][]map[string]any{"echo": {{"name": "a", "tags": []any{"instance:primary"}}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, cs[0]); !slices.Equal(got, []string{"from:echo", "instance:primary"}) {
		t.Fatalf("with its own instance tag: %v", got)
	}
	// Names that differ only in case would make one series.
	if _, err := registry.Configured(map[string][]map[string]any{"echo": {{"name": "Cache"}, {"name": "cache"}}}, nil, nil); err == nil || !strings.Contains(err.Error(), "same tag") {
		t.Fatalf("Cache and cache: %v", err)
	}
	// A discovered instance (NewInstance directly) gets none: its
	// container's tags tell it apart.
	c, err := registry.NewInstance("echo", 0, 1, map[string]any{"name": "redis-1"}, []string{"container_name:redis-1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, c); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "instance:") }) {
		t.Fatalf("a discovered instance tagged %v", got)
	}
}
