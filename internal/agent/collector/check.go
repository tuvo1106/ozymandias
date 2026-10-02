package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Check is a kind of collector that users configure: a factory, registered
// under a name, that turns one configured instance into a [Collector]. The
// built-in collectors (host, docker) are wired by the agent; a check is
// instantiated once per entry under collectors.checks.<name>.instances, or
// per container that asks for it with ozy.check.<name>.* labels
// (autodiscovery). The factory is the whole extension point: a new check is a
// package with a New(Instance) function and one line in the agent's registry.
type Check func(Instance) (Collector, error)

// Registry maps check names to their factories. It is a plain map, built by
// the agent and passed where needed, rather than a package-level table that
// init functions fill: which checks exist is then visible at the one place
// that builds it, and a test can use a registry of its own.
type Registry map[string]Check

// Names returns the registered check names, sorted.
func (r Registry) Names() []string { return slices.Sorted(maps.Keys(r)) }

// Instance is one configured instance of a check, as its factory sees it.
type Instance struct {
	// Check is the registered name ("redis").
	Check string
	// Name is the collector name the instance runs under, unique within the
	// agent: the check's name, or "<check>:<name>" when the instance has a
	// name (from its `name:` setting, or the container autodiscovery found
	// it on). It is the collector tag on the self-metrics.
	Name string
	// Settings are the instance's check-specific settings, with the common
	// keys (name, interval, tags) removed. Decode them with [Instance.Decode].
	Settings map[string]any
	// Discovered says the instance came from a container's labels, whose
	// tags already say which container it checks. A check passes the tags
	// that name its target by address through TargetTags, which leaves
	// them out for a discovered instance.
	Discovered bool
	Clock      clock.Clock
	Logger     *slog.Logger
}

// Decode decodes the instance's settings into v, a pointer to the check's
// config struct, strictly: a setting v has no field for is an error, so a
// misspelt key fails at startup rather than being silently ignored — the
// same rule the agent's own config follows.
//
// A setting given as text that looks like a list — a container label
// "[200, 301]", the only way a label can spell one — is read as a list
// when, and only when, v's field for it is a list. A label is text, and a
// password "[pw]" is text too; whether brackets mean a list is a question
// only the field's type answers.
func (i Instance) Decode(v any) error {
	settings, cloned := i.Settings, false
	if lists := listFields(reflect.TypeOf(v)); len(lists) > 0 {
		for k, val := range settings {
			n, ok := val.(*yaml.Node)
			if !ok || !lists[k] || n.Kind != yaml.ScalarNode || !strings.HasPrefix(n.Value, "[") {
				continue
			}
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(n.Value), &doc); err == nil && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.SequenceNode {
				if !cloned {
					settings, cloned = maps.Clone(i.Settings), true
				}
				settings[k] = doc.Content[0]
			}
		}
	}
	data, err := yaml.Marshal(settings)
	if err != nil {
		return fmt.Errorf("%s: re-encoding settings: %w", i.Name, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", i.Name, err)
	}
	return nil
}

// listFields names the settings of the config struct t points to whose
// fields are lists, by the key YAML decodes them from: the yaml tag's name,
// else the field name lower-cased (yaml.v3's rule); inline structs count
// as part of the outer one.
func listFields(t reflect.Type) map[string]bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	out := map[string]bool{}
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if strings.Contains(","+opts+",", ",inline,") {
			maps.Copy(out, listFields(f.Type))
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
			out[name] = true
		}
	}
	return out
}

// TargetTags returns tags, the ones a check uses to name its target by
// address (postgres' server:, http_check's url:), or nothing for a
// discovered instance. A discovered target's address is its container's,
// which changes when the container restarts, and every change would start
// a new series of what the container's tags already identify. One rule,
// here, rather than one special case per check that a new check could
// forget.
func (i Instance) TargetTags(tags ...string) []string {
	if i.Discovered {
		return nil
	}
	return tags
}

// Common instance settings, understood by every check and handled here
// rather than by each factory.
const (
	settingName     = "name"
	settingInterval = "interval"
	settingTags     = "tags"
)

// NewInstance builds the collector for one instance of check from its raw
// settings: it applies the common settings — name, interval (a duration
// string), tags (a list of "key:value") — and hands the rest to the factory.
//
// index is the instance's position in its list, used for the name when there
// are several unnamed instances of one check, so that each has its own
// self-metrics. extraTags are added to every metric after the instance's own
// (autodiscovery passes the container's tags).
func (r Registry) NewInstance(check string, index, of int, raw map[string]any, extraTags []string, clk clock.Clock, log *slog.Logger) (Collector, error) {
	return r.newInstance(check, index, of, raw, extraTags, false, clk, log)
}

// NewDiscovered builds the instance of check autodiscovery found on a
// container, tagged with tags (the container's); see Instance.Discovered.
func (r Registry) NewDiscovered(check string, raw map[string]any, tags []string, clk clock.Clock, log *slog.Logger) (Collector, error) {
	return r.newInstance(check, 0, 1, raw, tags, true, clk, log)
}

func (r Registry) newInstance(check string, index, of int, raw map[string]any, extraTags []string, discovered bool, clk clock.Clock, log *slog.Logger) (Collector, error) {
	factory, ok := r[check]
	if !ok {
		return nil, fmt.Errorf("check %q: no such check (have %s)", check, strings.Join(r.Names(), ", "))
	}
	settings := maps.Clone(raw)
	if settings == nil {
		settings = map[string]any{}
	}
	name := check
	if v, ok := settings[settingName]; ok {
		s, ok := text(v)
		if !ok || s == "" {
			return nil, fmt.Errorf("check %q instance %d: name must be a non-empty string", check, index)
		}
		name = check + ":" + s
	} else if of > 1 {
		name = fmt.Sprintf("%s:%d", check, index)
	}
	delete(settings, settingName)

	var iv time.Duration
	if v, ok := settings[settingInterval]; ok {
		s, _ := text(v)
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 || d%time.Second != 0 {
			return nil, fmt.Errorf("%s: interval %q: want a whole number of seconds, like 30s", name, s)
		}
		iv = d
	}
	delete(settings, settingInterval)

	var tags []string
	if v, ok := settings[settingTags]; ok {
		list, ok := v.([]any)
		if n, isNode := v.(*yaml.Node); isNode && n.Kind == yaml.SequenceNode {
			list, ok = make([]any, len(n.Content)), true
			for i, c := range n.Content {
				list[i] = c
			}
		}
		if !ok {
			return nil, fmt.Errorf("%s: tags must be a list of key:value strings", name)
		}
		for _, t := range list {
			s, ok := text(t)
			if !ok {
				return nil, fmt.Errorf("%s: tag %v is not a string", name, t)
			}
			n, ok := wire.NormalizeTag(s)
			if !ok {
				return nil, fmt.Errorf("%s: tag %q cannot be sent", name, s)
			}
			tags = append(tags, n)
		}
	}
	delete(settings, settingTags)
	tags = append(tags, extraTags...)

	if clk == nil {
		clk = clock.Real()
	}
	if log == nil {
		log = slog.Default()
	}
	c, err := factory(Instance{Check: check, Name: name, Settings: settings, Discovered: discovered, Clock: clk, Logger: log.With("check", name)})
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", name, err)
	}
	if iv == 0 {
		iv = c.Interval()
	}
	return &instance{name: name, iv: iv, tags: tags, inner: c}, nil
}

// text is a common setting's value as text: a string, or a scalar YAML
// node as written (configured instances keep their settings as nodes, so
// that a name like 007 is not the number 7).
func text(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case *yaml.Node:
		if v.Kind == yaml.ScalarNode && v.Tag != "!!null" {
			return v.Value, true
		}
	}
	return "", false
}

// instance is a check's collector under its instance name, interval and
// tags.
type instance struct {
	name  string
	iv    time.Duration
	tags  []string
	inner Collector
}

func (i *instance) Name() string            { return i.name }
func (i *instance) Interval() time.Duration { return i.iv }
func (i *instance) Collect(ctx context.Context, emit Emit) error {
	if len(i.tags) == 0 {
		return i.inner.Collect(ctx, emit)
	}
	return i.inner.Collect(ctx, func(m Metric) {
		// Kept, not merely added: the cap trims the check's own tags
		// first. A fresh slice: the check may reuse its slices.
		m.Keep = append(slices.Clip(m.Keep), i.tags...)
		emit(m)
	})
}

// Configured builds every instance under collectors.checks, in a stable
// order (by check name, then position), and returns every error rather than
// the first, so one startup shows every misconfigured instance.
//
// Each instance that has something to tell it apart — a name, or a
// position among several unnamed ones — gets the tag instance:<that>. Most
// checks name nothing about their target in their metrics (redis.mem.used
// is tagged only with the agent's host), so two redis instances would
// otherwise write the same series, and the store keeps one point per
// series and timestamp: one server's numbers would overwrite the other's.
// A single unnamed instance gets no tag; it has nothing to collide with.
// An instance whose own tags already say instance: keeps that one. Two
// names that normalise to one tag (Cache, cache) are refused like two
// equal names. Discovered instances (autodiscovery calls NewDiscovered,
// not this) carry their container's tags instead.
func (r Registry) Configured(checks map[string][]map[string]any, clk clock.Clock, log *slog.Logger) ([]Collector, error) {
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	sort.Strings(names)
	var (
		out     []Collector
		errs    []error
		seen    = map[string]bool{}
		seenTag = map[string]bool{}
	)
	for _, check := range names {
		list := checks[check]
		for i, raw := range list {
			c, err := r.NewInstance(check, i, len(list), raw, nil, clk, log)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if seen[c.Name()] {
				errs = append(errs, fmt.Errorf("check %s: two instances share this name", c.Name()))
				continue
			}
			seen[c.Name()] = true
			if err := tagInstance(c, check, seenTag); err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, c)
		}
	}
	return out, errors.Join(errs...)
}

// instanceTag is the tag key Configured adds to tell instances apart.
const instanceTag = "instance"

// tagInstance adds instance:<suffix> to c, a configured instance of check,
// when its name has a suffix (<check>:<suffix>) and its own tags do not
// already carry an instance tag. seen holds the tags given so far, per
// check, so two names that normalise alike are an error rather than one
// series.
func tagInstance(c Collector, check string, seen map[string]bool) error {
	in, ok := c.(*instance)
	if !ok {
		return nil
	}
	// An instance tag the instance's own tags give is taken as is, but
	// recorded like a derived one: tags [instance:b] on instance a beside
	// an instance named b would otherwise both say instance:b.
	for _, t := range in.tags {
		if k, _ := wire.SplitTag(t); k == instanceTag {
			if seen[check+"|"+t] {
				return fmt.Errorf("check %s: another instance already has the tag %s", in.name, t)
			}
			seen[check+"|"+t] = true
			return nil
		}
	}
	suffix, named := strings.CutPrefix(in.name, check+":")
	if !named {
		return nil
	}
	tag, ok := wire.NormalizeTag(instanceTag + ":" + suffix)
	if !ok {
		return fmt.Errorf("check %s: the name cannot be sent as a tag (%s:%s)", in.name, instanceTag, suffix)
	}
	if seen[check+"|"+tag] {
		return fmt.Errorf("check %s: another instance's name makes the same tag, %s", in.name, tag)
	}
	seen[check+"|"+tag] = true
	in.tags = append(in.tags, tag)
	return nil
}
