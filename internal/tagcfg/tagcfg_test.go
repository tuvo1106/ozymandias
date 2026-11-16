package tagcfg

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
)

func openMeta(t *testing.T) *meta.DB {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFilter_DenyDropsTheListedKeysAndAllowKeepsOnlyThem(t *testing.T) {
	c, err := Open(context.Background(), openMeta(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1, 0)
	if err := c.Set(ctx, "deny.m", Deny, []string{"user", "bare"}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "allow.m", Allow, []string{"env"}, now); err != nil {
		t.Fatal(err)
	}
	tags := []string{"bare", "env:prod", "user:u1", "user:u2"}
	for _, tc := range []struct {
		metric  string
		want    []string
		dropped int
	}{
		{"deny.m", []string{"env:prod"}, 3},  // a bare tag has the key "bare"
		{"allow.m", []string{"env:prod"}, 3}, // only env survives
		{"other", tags, 0},                   // no rule: untouched
		{"deny.m.longer", tags, 0},           // exact names only, not prefixes
	} {
		got, dropped := c.Filter(tc.metric, tags)
		if !reflect.DeepEqual(got, tc.want) || dropped != tc.dropped {
			t.Errorf("%s: got %v (dropped %d), want %v (%d)", tc.metric, got, dropped, tc.want, tc.dropped)
		}
	}
	if tags[0] != "bare" || len(tags) != 4 {
		t.Fatalf("the caller's slice was modified: %v", tags)
	}
	var none *Config
	if got, n := none.Filter("m", tags); n != 0 || len(got) != 4 {
		t.Fatal("a nil config must pass tags through")
	}
}

func TestConfig_RulesSurviveAReopenAndDeleteRemovesThem(t *testing.T) {
	db := openMeta(t)
	ctx := context.Background()
	c, _ := Open(ctx, db)
	if err := c.Set(ctx, "m", Deny, []string{"user"}, time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(ctx, db) // a restart
	if err != nil {
		t.Fatal(err)
	}
	if _, dropped := c2.Filter("m", []string{"user:1"}); dropped != 1 {
		t.Fatal("a saved rule was not applied after a reload")
	}
	if err := c2.Delete(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	if _, dropped := c2.Filter("m", []string{"user:1"}); dropped != 0 {
		t.Fatal("a deleted rule is still applied")
	}
	if rules, _ := c2.List(ctx); len(rules) != 0 {
		t.Fatalf("a deleted rule is still listed: %v", rules)
	}
	if err := c2.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("deleting a rule that does not exist: %v", err)
	}
}

func TestValidate_RefusesWhatCouldNeverMatchOrCouldHurt(t *testing.T) {
	long := make([]string, MaxKeys+1)
	for i := range long {
		long[i] = fmt.Sprint("k", i)
	}
	for name, c := range map[string]struct {
		metric, mode string
		keys         []string
	}{
		"no metric":       {"", Deny, []string{"a"}},
		"bad mode":        {"m", "block", []string{"a"}},
		"no keys":         {"m", Deny, nil},
		"too many keys":   {"m", Deny, long},
		"a key:value":     {"m", Deny, []string{"user:1"}},
		"a key with junk": {"m", Deny, []string{"us er"}},
		"an empty key":    {"m", Deny, []string{""}},
	} {
		if Validate(c.metric, c.mode, c.keys) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := Validate("m", Allow, []string{"env", "svc.name", "a/b-c_d"}); err != nil {
		t.Errorf("a good rule was refused: %v", err)
	}
}

// Reads are every intake request and writes are an operator's: hammering both under the
// race detector must neither race nor lose a write.
func TestConfig_ConcurrentReadsAndWritesAreSafe(t *testing.T) {
	c, _ := Open(context.Background(), openMeta(t))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = c.Set(context.Background(), fmt.Sprintf("m%d", i), Deny, []string{"user"}, time.Unix(1, 0))
				c.Filter(fmt.Sprintf("m%d", (i+1)%4), []string{"user:1"})
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		if _, dropped := c.Filter(fmt.Sprintf("m%d", i), []string{"user:1"}); dropped != 1 {
			t.Errorf("rule m%d was lost", i)
		}
	}
}
