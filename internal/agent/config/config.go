package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	base "github.com/tuvo1106/ozymandias/internal/config"
)

// EnvPrefix prefixes every agent environment override, e.g.
// OZY_AGENT_INTAKE_URL.
const EnvPrefix = "OZY_AGENT_"

// Intake says where the agent forwards what it collects.
type Intake struct {
	// URL is ozyd's base URL; the agent POSTs to /v1/* under it.
	URL string `yaml:"url"`
}

// Agent is the agent's configuration.
type Agent struct {
	HTTP   base.HTTP `yaml:"http"`
	Intake Intake    `yaml:"intake"`
	// Hostname is the value of the host tag on everything this agent sends.
	// Empty means the OS hostname. Set it explicitly in containers, where the
	// OS hostname is a meaningless container id.
	Hostname string `yaml:"hostname"`
	// Tags are added to everything this agent sends, e.g. env:dev. Fragments
	// append to this list rather than replacing it.
	Tags []string `yaml:"tags"`
	// ConfdPath is the conf.d directory of per-app fragments. Relative paths
	// are resolved against the working directory.
	ConfdPath string   `yaml:"confd_path"`
	Log       base.Log `yaml:"log"`
}

// Default returns the settings a bare `agent` runs with.
func Default() Agent {
	return Agent{
		HTTP:      base.HTTP{Addr: ":8126", ShutdownTimeout: 10 * time.Second},
		Intake:    Intake{URL: "http://localhost:9400"},
		ConfdPath: "./deploy/agent.d",
		Log:       base.Log{Level: "info", Format: "text"},
	}
}

// Validate checks every section.
func (a *Agent) Validate() error {
	errs := []error{a.HTTP.Validate(), a.Log.Validate()}
	if u, err := url.Parse(a.Intake.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("intake.url %q: want an absolute http(s) URL", a.Intake.URL))
	}
	for _, t := range a.Tags {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, ",|\n") {
			errs = append(errs, fmt.Errorf("tags: %q is empty or contains ',', '|' or a newline", t))
		}
	}
	return errors.Join(errs...)
}

// Load reads the agent config from path (empty: defaults only), the conf.d
// directory it names, and the OZY_AGENT_* environment.
func Load(path string, environ []string) (Agent, []string, error) {
	// Phase 1: where is conf.d? Main file + env only; validation waits for
	// phase 2, when every layer is in.
	probe := Default()
	if _, err := base.Load(&probe, base.Options{
		Path: path, EnvPrefix: EnvPrefix, Environ: environ, SkipValidate: true,
	}); err != nil {
		return Agent{}, nil, err
	}
	// A fragment cannot move the directory it was loaded from (the directory
	// was chosen before any fragment was read). Checked on the fragments
	// alone, since env would otherwise mask it.
	var fragOnly Agent
	if _, err := base.Load(&fragOnly, base.Options{FragmentDir: probe.ConfdPath, SkipValidate: true}); err != nil {
		return Agent{}, nil, err
	}
	if fragOnly.ConfdPath != "" {
		return Agent{}, nil, fmt.Errorf("config: confd_path cannot be set inside a conf.d fragment (found %q)", fragOnly.ConfdPath)
	}
	// Phase 2: everything, in precedence order.
	cfg := Default()
	warnings, err := base.Load(&cfg, base.Options{
		Path:        path,
		FragmentDir: probe.ConfdPath,
		EnvPrefix:   EnvPrefix,
		Environ:     environ,
	})
	if err != nil {
		return Agent{}, warnings, err
	}
	return cfg, warnings, nil
}
