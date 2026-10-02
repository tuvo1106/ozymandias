package collector

import (
	"strings"
	"testing"
)

func TestInt_AndPort(t *testing.T) {
	var cfg struct {
		N Int  `yaml:"n"`
		P Port `yaml:"p"`
	}
	inst := Instance{Name: "x", Settings: map[string]any{"n": "-3", "p": 6379}}
	if err := inst.Decode(&cfg); err != nil || cfg.N != -3 || cfg.P != 6379 {
		t.Fatalf("%+v %v", cfg, err)
	}
	inst.Settings = map[string]any{"n": 7, "p": "5432"}
	if err := inst.Decode(&cfg); err != nil || cfg.N != 7 || cfg.P != 5432 {
		t.Fatalf("%+v %v", cfg, err)
	}
	for name, tc := range map[string]struct {
		settings map[string]any
		msg      string
	}{
		"int not a number": {map[string]any{"n": "six"}, "not a whole number"},
		"int a list":       {map[string]any{"n": []any{1}}, "want a whole number"},
		"int a float":      {map[string]any{"n": 1.5}, "not a whole number"},
		"port zero":        {map[string]any{"p": 0}, "1 to 65535"},
		"port too big":     {map[string]any{"p": "70000"}, "1 to 65535"},
		"port a word":      {map[string]any{"p": "http"}, `p: "http" is not a whole number`},
		"port a map":       {map[string]any{"p": map[string]any{"a": 1}}, "p: want a whole number"},
	} {
		inst.Settings = tc.settings
		if err := inst.Decode(&cfg); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.msg)
		}
	}
}
