package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func TestLoad_DefaultsOnlyAreValid(t *testing.T) {
	// Point conf.d somewhere empty so the repo's own deploy/agent.d (relative
	// to the working directory) cannot leak into the test.
	cfg, warnings, err := Load("", []string{"OZY_AGENT_CONFD_PATH=" + t.TempDir()})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if cfg.HTTP.Addr != ":8126" || cfg.Intake.URL != "http://localhost:9400" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoad_ReferenceFileLoads(t *testing.T) {
	if _, _, err := Load("../../../deploy/agent.yaml", []string{"OZY_AGENT_CONFD_PATH=" + t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

// Precedence: defaults < file < fragments < env, for every key.
func TestLoad_FragmentsFromTheConfiguredDirectoryAndEnvWins(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{
		"agent.yaml":          "tags: [env:dev]\nconfd_path: CONFD\n",
		"frags/app-a.yaml":    "tags: [app:a]\n",
		"frags/app-b.yaml":    "tags: [app:b]\nhostname: from-fragment\n",
		"frags/notes.txt":     "not yaml",
		"frags/nested/x.yaml": "tags: [not-read]\n",
	})
	testutil.WriteFiles(t, dir, map[string]string{
		"agent.yaml": "tags: [env:dev]\nconfd_path: " + dir + "/frags\n",
	})
	cfg, _, err := Load(dir+"/agent.yaml", []string{"OZY_AGENT_HOSTNAME=from-env"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"env:dev", "app:a", "app:b"}; !reflect.DeepEqual(cfg.Tags, want) {
		t.Errorf("tags = %v, want %v", cfg.Tags, want)
	}
	if cfg.Hostname != "from-env" {
		t.Errorf("hostname = %q, want the env value to beat the fragment", cfg.Hostname)
	}
}

func TestLoad_ConfdPathFromEnvironment(t *testing.T) {
	frags := testutil.TempDirWith(t, map[string]string{"x.yaml": "tags: [from:env-dir]\n"})
	cfg, _, err := Load("", []string{"OZY_AGENT_CONFD_PATH=" + frags})
	if err != nil || len(cfg.Tags) != 1 || cfg.Tags[0] != "from:env-dir" {
		t.Fatalf("tags = %v, err = %v", cfg.Tags, err)
	}
}

// A fragment can't move the directory it was loaded from.
func TestLoad_ConfdPathInsideFragmentIsAnError(t *testing.T) {
	frags := testutil.TempDirWith(t, map[string]string{"x.yaml": "confd_path: /elsewhere\n"})
	_, _, err := Load("", []string{"OZY_AGENT_CONFD_PATH=" + frags})
	if err == nil || !strings.Contains(err.Error(), "confd_path") {
		t.Fatalf("err = %v", err)
	}
}

// The SDKs' OZY_AGENT_HOST shares the prefix: it must warn, not fail.
func TestLoad_SDKVariableSharingThePrefixOnlyWarns(t *testing.T) {
	_, warnings, err := Load("", []string{"OZY_AGENT_HOST=localhost", "OZY_AGENT_CONFD_PATH=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "OZY_AGENT_HOST") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestLoad_MainFileErrorsSurfaceFromTheFirstPass(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"agent.yaml": "bogus: 1\n"})
	if _, _, err := Load(dir+"/agent.yaml", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_FragmentErrorsSurface(t *testing.T) {
	frags := testutil.TempDirWith(t, map[string]string{"x.yaml": "bogus: 1\n"})
	if _, _, err := Load("", []string{"OZY_AGENT_CONFD_PATH=" + frags}); err == nil {
		t.Fatal("want error")
	}
}

func TestAgent_Validate(t *testing.T) {
	cases := map[string]func(*Agent){
		"relative intake url": func(a *Agent) { a.Intake.URL = "/v1" },
		"ftp intake url":      func(a *Agent) { a.Intake.URL = "ftp://x" },
		"unparseable url":     func(a *Agent) { a.Intake.URL = "http://[::1" },
		"empty tag":           func(a *Agent) { a.Tags = []string{" "} },
		"tag with comma":      func(a *Agent) { a.Tags = []string{"a:b,c"} },
		"tag with pipe":       func(a *Agent) { a.Tags = []string{"a|b"} },
		"bad log level":       func(a *Agent) { a.Log.Level = "chatty" },
		"bad http addr":       func(a *Agent) { a.HTTP.Addr = "8126" },
	}
	for name, mutate := range cases {
		a := Default()
		mutate(&a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	a := Default()
	a.Tags = []string{"env:dev", "bare"}
	if err := a.Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
}
