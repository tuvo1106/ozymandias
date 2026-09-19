package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Validator is implemented by config structs that can check their own
// invariants. [Load] calls it after all layers are applied.
type Validator interface {
	Validate() error
}

// Options says where [Load] finds each layer.
type Options struct {
	// Path is the main YAML file. Empty means defaults (and env) only.
	Path string
	// FragmentDir is a conf.d directory whose *.yaml files are merged over
	// the main file. Empty means none. A missing directory is not an error:
	// it is how "no app-specific config yet" looks.
	FragmentDir string
	// EnvPrefix is prepended to every environment variable name, e.g.
	// "OZY_". Empty disables environment overrides.
	EnvPrefix string
	// Environ is the environment as "KEY=value" pairs; os.Environ() in
	// production, a literal in tests.
	Environ []string
	// SkipValidate loads without calling Validate — for a first pass that
	// only needs to read one setting (the agent's confd_path) before the real
	// load.
	SkipValidate bool
}

// Load fills cfg, which must be a pointer to a struct already holding its
// defaults, from the layers described in the package documentation. It
// returns warnings (non-fatal findings, e.g. unknown environment variables)
// for the caller to log.
func Load(cfg any, opts Options) (warnings []string, err error) {
	rv := reflect.ValueOf(cfg)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("config: Load needs a pointer to a struct, got %T", cfg)
	}

	merged := map[string]any{}
	origins := map[string]string{}
	if opts.Path != "" {
		if err := mergeFile(cfg, opts.Path, merged, origins); err != nil {
			return nil, err
		}
	}
	if opts.FragmentDir != "" {
		files, err := fragmentFiles(opts.FragmentDir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if err := mergeFile(cfg, f, merged, origins); err != nil {
				return nil, err
			}
		}
	}
	if len(merged) > 0 {
		if err := decodeInto(cfg, merged); err != nil {
			return nil, err
		}
	}
	if opts.EnvPrefix != "" {
		warnings, err = applyEnv(rv.Elem(), opts.EnvPrefix, opts.Environ)
		if err != nil {
			return nil, err
		}
	}
	if v, ok := cfg.(Validator); ok && !opts.SkipValidate {
		if err := v.Validate(); err != nil {
			return warnings, fmt.Errorf("config: %w", err)
		}
	}
	return warnings, nil
}

// mergeFile strictly validates one YAML file against the config's shape (so
// errors carry that file's line numbers) and deep-merges it into merged.
func mergeFile(cfg any, path string, merged map[string]any, origins map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	// Strict pass: decode into a scratch copy with unknown fields rejected.
	scratch := reflect.New(reflect.TypeOf(cfg).Elem()).Interface()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(scratch); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // only comments: an empty file, e.g. a placeholder fragment
		}
		return fmt.Errorf("config: %s: %w", path, err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(data, &tree); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return deepMerge(merged, tree, "", path, origins)
}

// deepMerge merges src into dst: maps recurse, lists append, and a scalar
// already set by an earlier file is a conflict. A null value (`tags:` with
// nothing under it) counts as absent on either side, so commenting out every
// item of a list neither clobbers nor blocks another file's items. origins
// records which file set each scalar path, for the conflict message.
func deepMerge(dst, src map[string]any, prefix, file string, origins map[string]string) error {
	for k, sv := range src {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if sv == nil {
			continue
		}
		dv, exists := dst[k]
		if !exists || dv == nil {
			dst[k] = sv
			recordOrigins(sv, path, file, origins)
			continue
		}
		switch s := sv.(type) {
		case map[string]any:
			d, ok := dv.(map[string]any)
			if !ok {
				return fmt.Errorf("config: %s: %q is a mapping here but not in %s", file, path, origins[path])
			}
			if err := deepMerge(d, s, path, file, origins); err != nil {
				return err
			}
		case []any:
			d, ok := dv.([]any)
			if !ok {
				return fmt.Errorf("config: %s: %q is a list here but not in %s", file, path, origins[path])
			}
			dst[k] = append(d, s...)
		default:
			return fmt.Errorf("config: %s: %q is already set in %s; a setting may be defined in only one file", file, path, origins[path])
		}
	}
	return nil
}

func recordOrigins(v any, path, file string, origins map[string]string) {
	origins[path] = file
	if m, ok := v.(map[string]any); ok {
		for k, sub := range m {
			recordOrigins(sub, path+"."+k, file, origins)
		}
	}
}

func decodeInto(cfg any, merged map[string]any) error {
	data, err := yaml.Marshal(merged)
	if err != nil {
		return fmt.Errorf("config: re-encoding merged config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func fragmentFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: reading fragment dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.Type().IsRegular() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

var durationType = reflect.TypeOf(time.Duration(0))

// applyEnv overrides leaves of the struct v from environment variables named
// prefix + the upper-cased YAML path. It returns a warning per variable that
// carries the prefix but matches no setting.
func applyEnv(v reflect.Value, prefix string, environ []string) ([]string, error) {
	env := map[string]string{}
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, prefix) {
			env[k] = val
		}
	}
	used := map[string]bool{}
	if err := walkEnv(v, prefix, env, used); err != nil {
		return nil, err
	}
	var warnings []string
	for k := range env {
		if !used[k] {
			warnings = append(warnings, fmt.Sprintf("environment variable %s matches no setting and was ignored", k))
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}

func walkEnv(v reflect.Value, prefix string, env map[string]string, used map[string]bool) error {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		envName := prefix + strings.ToUpper(name)
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct {
			if err := walkEnv(fv, envName+"_", env, used); err != nil {
				return err
			}
			continue
		}
		raw, ok := env[envName]
		if !ok {
			continue
		}
		used[envName] = true
		if err := setFromString(fv, raw); err != nil {
			return fmt.Errorf("config: %s=%q: %w", envName, raw, err)
		}
	}
	return nil
}

// setFromString parses raw into a leaf field. Lists are comma-separated,
// with surrounding space trimmed and empty items dropped.
func setFromString(fv reflect.Value, raw string) error {
	if fv.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(f)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("lists of %s cannot be set from the environment; use the YAML file", fv.Type().Elem())
		}
		var items []string
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		fv.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("type %s cannot be set from the environment", fv.Type())
	}
	return nil
}
