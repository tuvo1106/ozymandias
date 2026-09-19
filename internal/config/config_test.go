package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func TestLoadOzyd_DefaultsOnlyAreValid(t *testing.T) {
	cfg, warnings, err := LoadOzyd("", nil)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if !reflect.DeepEqual(cfg, DefaultOzyd()) {
		t.Fatalf("cfg = %+v, want defaults", cfg)
	}
}

// The reference file is documentation *and* the config compose runs with; it
// must always load.
func TestLoadOzyd_ReferenceFileLoads(t *testing.T) {
	if _, _, err := LoadOzyd("../../deploy/ozyd.yaml", nil); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_FileOverridesOnlyTheKeysItSets(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"d.yaml": `
http:
  addr: "127.0.0.1:1234"
log:
  format: json
provisioning:
  paths: [a, b]
`})
	cfg, _, err := LoadOzyd(dir+"/d.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:1234" || cfg.Log.Format != "json" {
		t.Errorf("file values not applied: %+v", cfg)
	}
	// Siblings of overridden keys keep their defaults.
	if cfg.HTTP.ShutdownTimeout != 10*time.Second || cfg.Log.Level != "info" || cfg.DataDir != "./data/ozyd" {
		t.Errorf("defaults lost: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Provisioning.Paths, []string{"a", "b"}) {
		t.Errorf("paths = %v", cfg.Provisioning.Paths)
	}
}

func TestLoad_ParsesDurationsFromYAML(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"d.yaml": "http:\n  shutdown_timeout: 1m30s\n"})
	cfg, _, err := LoadOzyd(dir+"/d.yaml", nil)
	if err != nil || cfg.HTTP.ShutdownTimeout != 90*time.Second {
		t.Fatalf("timeout = %v, err = %v", cfg.HTTP.ShutdownTimeout, err)
	}
}

// A typo must fail loudly with a location, not fall back to a default.
func TestLoad_UnknownKeyInFileIsAnErrorWithFileAndLine(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"d.yaml": "http:\n  adr: \":1\"\n"})
	_, _, err := LoadOzyd(dir+"/d.yaml", nil)
	if err == nil || !strings.Contains(err.Error(), "d.yaml") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want one naming d.yaml line 2", err)
	}
}

func TestLoad_FileErrors(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{
		"bad.yaml":   "http: [unclosed\n",
		"empty.yaml": "  \n",
	})
	if _, _, err := LoadOzyd(dir+"/missing.yaml", nil); err == nil {
		t.Error("missing file: want error")
	}
	if _, _, err := LoadOzyd(dir+"/bad.yaml", nil); err == nil {
		t.Error("invalid YAML: want error")
	}
	if _, _, err := LoadOzyd(dir+"/empty.yaml", nil); err != nil {
		t.Errorf("empty file: %v, want defaults", err)
	}
}

func TestLoad_EnvironmentOverridesFile(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"d.yaml": "http:\n  addr: \":1\"\n"})
	env := []string{
		"OZY_HTTP_ADDR=:2",
		"OZY_HTTP_SHUTDOWN_TIMEOUT=3s",
		"OZY_PROVISIONING_PATHS= x , ,y ",
		"UNRELATED=1",
	}
	cfg, warnings, err := LoadOzyd(dir+"/d.yaml", env)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if cfg.HTTP.Addr != ":2" || cfg.HTTP.ShutdownTimeout != 3*time.Second {
		t.Errorf("env not applied: %+v", cfg.HTTP)
	}
	if !reflect.DeepEqual(cfg.Provisioning.Paths, []string{"x", "y"}) {
		t.Errorf("paths = %q, want [x y]", cfg.Provisioning.Paths)
	}
}

// The environment is shared with other software, so unknown names warn
// rather than fail.
func TestLoad_UnknownPrefixedEnvIsAWarning(t *testing.T) {
	_, warnings, err := LoadOzyd("", []string{"OZY_HTTP_ADR=:1", "OZY_NOPE=x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "OZY_HTTP_ADR") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestLoad_BadEnvValueIsAnError(t *testing.T) {
	_, _, err := LoadOzyd("", []string{"OZY_HTTP_SHUTDOWN_TIMEOUT=soon"})
	if err == nil || !strings.Contains(err.Error(), "OZY_HTTP_SHUTDOWN_TIMEOUT") {
		t.Fatalf("err = %v", err)
	}
}

// kinds covers every leaf type the environment loader supports.
type kinds struct {
	S   string        `yaml:"s"`
	B   bool          `yaml:"b"`
	I   int           `yaml:"i"`
	F   float64       `yaml:"f"`
	D   time.Duration `yaml:"d"`
	L   []string      `yaml:"l"`
	N   []int         `yaml:"n"`
	M   map[string]string
	Skp string `yaml:"-"`
	Sub struct {
		X string `yaml:"x"`
	} `yaml:"sub"`
}

func TestLoad_EnvSupportsEachLeafType(t *testing.T) {
	var k kinds
	env := []string{"T_S=s", "T_B=true", "T_I=-7", "T_F=2.5", "T_D=1h", "T_L=a,b", "T_SUB_X=deep"}
	if _, err := Load(&k, Options{EnvPrefix: "T_", Environ: env}); err != nil {
		t.Fatal(err)
	}
	if k.S != "s" || !k.B || k.I != -7 || k.F != 2.5 || k.D != time.Hour || len(k.L) != 2 || k.Sub.X != "deep" {
		t.Fatalf("got %+v", k)
	}
}

func TestLoad_EnvRejectsUnparseableAndUnsupported(t *testing.T) {
	for _, kv := range []string{"T_B=maybe", "T_I=one", "T_F=x", "T_D=5", "T_N=1,2"} {
		var k kinds
		if _, err := Load(&k, Options{EnvPrefix: "T_", Environ: []string{kv}}); err == nil {
			t.Errorf("%s: want error", kv)
		}
	}
}

func TestLoad_RequiresPointerToStruct(t *testing.T) {
	var s string
	if _, err := Load(s, Options{}); err == nil {
		t.Error("non-pointer: want error")
	}
	if _, err := Load(&s, Options{}); err == nil {
		t.Error("pointer to non-struct: want error")
	}
}

func TestOzyd_ValidateCollectsEveryProblem(t *testing.T) {
	cfg := DefaultOzyd()
	cfg.HTTP.Addr = "no-port"
	cfg.HTTP.ShutdownTimeout = 0
	cfg.Log.Level = "loud"
	cfg.Log.Format = "xml"
	cfg.DataDir = ""
	cfg.Provisioning.Paths = []string{"ok", ""}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"http.addr", "shutdown_timeout", "log.level", "log.format", "data_dir", "provisioning.paths[1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoad_ValidationFailureIsReported(t *testing.T) {
	if _, _, err := LoadOzyd("", []string{"OZY_LOG_LEVEL=loud"}); err == nil {
		t.Fatal("want validation error")
	}
}

func TestLog_SlogLevel(t *testing.T) {
	for level, want := range map[string]string{"debug": "DEBUG", "info": "INFO", "warn": "WARN", "error": "ERROR"} {
		if got := (Log{Level: level}).SlogLevel().String(); got != want {
			t.Errorf("%s → %s, want %s", level, got, want)
		}
	}
}
