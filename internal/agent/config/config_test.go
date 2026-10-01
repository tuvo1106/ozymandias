package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
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
		"hostname with comma": func(a *Agent) { a.Hostname = "mac,mini" },
		// A count's interval is whole seconds on the wire.
		"fractional collectors.interval": func(a *Agent) { a.Collectors.Interval = 1500 * time.Millisecond },
		"zero collectors.interval":       func(a *Agent) { a.Collectors.Interval = 0 },
		"sub-second collectors.interval": func(a *Agent) { a.Collectors.Interval = 500 * time.Millisecond },
		"zero collectors.timeout":        func(a *Agent) { a.Collectors.Timeout = 0 },
		"negative collectors.timeout":    func(a *Agent) { a.Collectors.Timeout = -time.Second },
		"fractional host.interval":       func(a *Agent) { a.Collectors.Host.Interval = 2500 * time.Millisecond },
		"bad exclude pattern":            func(a *Agent) { a.Collectors.Host.ExcludeInterfaces = []string{"("} },
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
	a.Collectors.Interval = time.Second
	a.Collectors.Host.Interval = 0 // 0 means collectors.interval
	a.Collectors.Host.ExcludeInterfaces = []string{`^veth`}
	if err := a.Validate(); err != nil {
		t.Errorf("valid config: %v", err)
	}
	a.Collectors.Host.Interval = time.Minute
	if err := a.Validate(); err != nil {
		t.Errorf("host.interval 1m: %v", err)
	}
}

// The default interface exclusions: virtual interfaces of macOS and of
// Docker on Linux, but never a real NIC or loopback.
func TestDefaultExcludeInterfaces(t *testing.T) {
	rx, err := HostCollector{ExcludeInterfaces: DefaultExcludeInterfaces}.Excludes()
	if err != nil {
		t.Fatal(err)
	}
	excluded := func(name string) bool {
		for _, r := range rx {
			if r.MatchString(name) {
				return true
			}
		}
		return false
	}
	for _, n := range []string{"utun3", "awdl0", "llw0", "anpi1", "gif0", "stf0", "veth1a2b3c4", "docker0", "br-9f8e7d6c5b4a"} {
		if !excluded(n) {
			t.Errorf("%s is reported", n)
		}
	}
	for _, n := range []string{"eth0", "en0", "lo", "lo0", "wlan0", "enp3s0", "bridge0"} {
		if excluded(n) {
			t.Errorf("%s is skipped", n)
		}
	}
}

// The shipped fragments load over the reference file, as compose runs them,
// and the judge rule is in effect.
func TestLoad_ShippedFragments(t *testing.T) {
	cfg, warnings, err := Load("../../../deploy/agent.yaml", []string{"OZY_AGENT_CONFD_PATH=../../../deploy/agent.d"})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if n := cfg.Collectors.Docker.AutodiscoveryNetwork; n != "ozymandias" {
		t.Errorf("autodiscovery_network = %q, want the stack's network", n)
	}
	rules := cfg.Collectors.Docker.ContainerNameRewrite
	if len(rules) != 2 || rules[1].Match != "^judge-.*" || rules[1].Replace != "judge" {
		t.Fatalf("container_name_rewrite = %+v", rules)
	}
	// The stack's own rule keeps the name (it is there to drop the id).
	stack := regexp.MustCompile(rules[0].Match)
	for _, name := range []string{"ozymandias-gid-probe", "ozy-smoke-long", "ozy-smoke-redis"} {
		if !stack.MatchString(name) || stack.ReplaceAllString(name, rules[0].Replace) != name {
			t.Errorf("%s: matched %v, rewritten to %q", name, stack.MatchString(name), stack.ReplaceAllString(name, rules[0].Replace))
		}
	}
}

func TestAgent_ValidateDocker(t *testing.T) {
	for name, mutate := range map[string]func(*Agent){
		"empty rewrite match": func(a *Agent) { a.Collectors.Docker.ContainerNameRewrite = []NameRewrite{{Replace: "x"}} },
		"bad rewrite match":   func(a *Agent) { a.Collectors.Docker.ContainerNameRewrite = []NameRewrite{{Match: "(", Replace: "x"}} },
		"empty replace":       func(a *Agent) { a.Collectors.Docker.ContainerNameRewrite = []NameRewrite{{Match: "^judge-"}} },
		"replace names a missing group": func(a *Agent) {
			a.Collectors.Docker.ContainerNameRewrite = []NameRewrite{{Match: "^judge-(.*)$", Replace: "$1_sandbox"}}
		},
		"replace numbers a missing group": func(a *Agent) {
			a.Collectors.Docker.ContainerNameRewrite = []NameRewrite{{Match: "^judge-(.*)$", Replace: "${2}"}}
		},
		"no socket":           func(a *Agent) { a.Collectors.Docker.Socket = "" },
		"zero concurrency":    func(a *Agent) { a.Collectors.Docker.MaxConcurrency = 0 },
		"fractional interval": func(a *Agent) { a.Collectors.Docker.Interval = 1500 * time.Millisecond },
	} {
		a := Default()
		mutate(&a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	a := Default()
	a.Collectors.Docker.Enabled, a.Collectors.Docker.Socket = false, ""
	if err := a.Validate(); err != nil {
		t.Errorf("a disabled collector needs no socket: %v", err)
	}
}

// Replacements that reference only groups the pattern has are accepted.
func TestRewrites_AcceptsGoodTemplates(t *testing.T) {
	for _, r := range []NameRewrite{
		{Match: "^judge-", Replace: "judge"},
		{Match: "^(ozy-smoke-[a-z]+)$", Replace: "${1}"},
		{Match: "^(?P<app>[a-z]+)-[0-9]+$", Replace: "${app}"},
		{Match: "^(a)(b)$", Replace: "$1-$2"},
		{Match: "^x$", Replace: "cost$$"},
	} {
		d := DockerCollector{ContainerNameRewrite: []NameRewrite{r}}
		if rw, err := d.Rewrites(); err != nil || len(rw) != 1 || rw[0].Replace != r.Replace {
			t.Errorf("%+v: %v, %v", r, rw, err)
		}
	}
}

// Review finding: a configured check's settings went through YAML's typing
// of plain scalars, so a password 0123 reached the check as "83" and a
// database name 1e3 as "1000". The check gets the text as written, and the
// common settings still read as text (a name 007 is not 7).
func TestCheckSettings_KeepTheirText(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"agent.yaml": `
collectors:
  checks:
    probe:
      instances:
        - name: 007
          interval: 30s
          tags: [env:prod]
          password: 0123
          db: 1e3
          user: 2024-01-02
          hex: 0x1F
          port: 6379
          empty:
`})
	cfg, _, err := Load(dir+"/agent.yaml", []string{"OZY_AGENT_CONFD_PATH=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	type probeCfg struct {
		Password string `yaml:"password"`
		DB       string `yaml:"db"`
		User     string `yaml:"user"`
		Hex      string `yaml:"hex"`
		Port     int    `yaml:"port"`
	}
	var got probeCfg
	reg := collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
		return nil, inst.Decode(&got)
	}}
	list, err := reg.Configured(cfg.Collectors.Instances(), nil, nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("%v: %v", list, err)
	}
	want := probeCfg{Password: "0123", DB: "1e3", User: "2024-01-02", Hex: "0x1F", Port: 6379}
	if got != want {
		t.Fatalf("the check got %+v, want %+v", got, want)
	}
	if c := list[0]; c.Name() != "probe:007" || c.Interval() != 30*time.Second {
		t.Fatalf("instance %s every %v", c.Name(), c.Interval())
	}
}

// Review finding: Check decodes itself, and a nested decode does not
// inherit strictness, so a misspelt instances: loaded as no check at all.
func TestLoad_AMisspeltCheckKeyFails(t *testing.T) {
	dir := testutil.TempDirWith(t, map[string]string{"agent.yaml": "collectors:\n  checks:\n    redis:\n      instnaces:\n        - host: x\n"})
	_, _, err := Load(dir+"/agent.yaml", []string{"OZY_AGENT_CONFD_PATH=" + t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "instnaces") || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("err = %v", err)
	}
}
