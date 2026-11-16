package tagcfg

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
)

// Modes.
const (
	Deny  = "deny"
	Allow = "allow"
)

// Limits on what a rule may say. A tag key longer than the wire allows can never
// match, and an unbounded list is a way to make intake slow.
const (
	MaxKeys      = 64
	maxKeyLength = 200
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// Store is the persistence the rules need.
type Store interface {
	SetTagConfig(ctx context.Context, c meta.TagConfig, now time.Time) error
	DeleteTagConfig(ctx context.Context, metric string) error
	TagConfigs(ctx context.Context) ([]meta.TagConfig, error)
}

type rule struct {
	mode string
	keys map[string]struct{}
}

// Config is the live set of rules. Reads (every intake request) are one atomic load of
// an immutable map; writes build a new map, save it, then swap it in.
type Config struct {
	store Store
	rules atomic.Pointer[map[string]rule]
	write chan struct{} // capacity 1: one writer at a time, without holding a lock across SQLite
}

// Open loads the saved rules.
func Open(ctx context.Context, store Store) (*Config, error) {
	c := &Config{store: store, write: make(chan struct{}, 1)}
	saved, err := store.TagConfigs(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]rule, len(saved))
	for _, s := range saved {
		m[s.Metric] = newRule(s.Mode, s.Keys)
	}
	c.rules.Store(&m)
	return c, nil
}

func newRule(mode string, keys []string) rule {
	r := rule{mode: mode, keys: make(map[string]struct{}, len(keys))}
	for _, k := range keys {
		r.keys[k] = struct{}{}
	}
	return r
}

// Validate says whether a rule can be saved. The error is for the person who wrote it.
func Validate(metric, mode string, keys []string) error {
	switch {
	case strings.TrimSpace(metric) == "" || len(metric) > maxKeyLength:
		return fmt.Errorf("metric must be 1-%d characters", maxKeyLength)
	case mode != Deny && mode != Allow:
		return fmt.Errorf("mode %q: want deny or allow", mode)
	case len(keys) == 0 || len(keys) > MaxKeys:
		return fmt.Errorf("keys: want 1-%d tag keys", MaxKeys)
	}
	for _, k := range keys {
		if len(k) > maxKeyLength || !keyPattern.MatchString(k) {
			return fmt.Errorf("key %q: want letters, digits, and _ . / - only (a key, not a key:value)", k)
		}
	}
	return nil
}

// Filter applies the metric's rule to a tag list and says how many tags it dropped. A
// metric with no rule gets its tags back untouched, with no allocation.
func (c *Config) Filter(metric string, tags []string) (kept []string, dropped int) {
	if c == nil {
		return tags, 0
	}
	r, ok := (*c.rules.Load())[metric]
	if !ok {
		return tags, 0
	}
	kept = make([]string, 0, len(tags))
	for _, t := range tags {
		key, _, _ := strings.Cut(t, ":")
		_, listed := r.keys[key]
		if listed == (r.mode == Allow) { // allow: keep the listed; deny: keep the unlisted
			kept = append(kept, t)
		}
	}
	return kept, len(tags) - len(kept)
}

// List returns every rule, by metric.
func (c *Config) List(ctx context.Context) ([]meta.TagConfig, error) { return c.store.TagConfigs(ctx) }

func (c *Config) lock(ctx context.Context) error {
	select {
	case c.write <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Set saves a rule and applies it from the next request on.
func (c *Config) Set(ctx context.Context, metric, mode string, keys []string, now time.Time) error {
	if err := Validate(metric, mode, keys); err != nil {
		return err
	}
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.write }()
	keys = append([]string(nil), keys...)
	sort.Strings(keys)
	if err := c.store.SetTagConfig(ctx, meta.TagConfig{Metric: metric, Mode: mode, Keys: keys}, now); err != nil {
		return err
	}
	c.swap(func(m map[string]rule) { m[metric] = newRule(mode, keys) })
	return nil
}

// Delete removes a rule.
func (c *Config) Delete(ctx context.Context, metric string) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer func() { <-c.write }()
	if err := c.store.DeleteTagConfig(ctx, metric); err != nil {
		return err
	}
	c.swap(func(m map[string]rule) { delete(m, metric) })
	return nil
}

// swap installs a modified copy of the rules. Callers hold the write slot.
func (c *Config) swap(edit func(map[string]rule)) {
	old := *c.rules.Load()
	next := make(map[string]rule, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	edit(next)
	c.rules.Store(&next)
}
