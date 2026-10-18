package monitor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// TestShippedMonitorsLoad: every file in deploy/monitors/ provisions, and
// mentions only notifiers the reference config has. A shipped monitor that
// fails to load is one nobody will know is missing.
func TestShippedMonitorsLoad(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dir := filepath.Join("..", "..", "deploy", "monitors")
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	res := Provision(context.Background(), provisionDB{db}, []string{dir}, []string{"log", "hook"}, t0, nil)
	if res.Failed != 0 || res.Loaded != len(files) || len(files) == 0 {
		t.Fatalf("%+v of %d files", res, len(files))
	}
	rows, _ := db.Monitors(context.Background())
	for _, r := range rows {
		for _, m := range spec.Mentions(r.Message) {
			if m != "log" && m != "hook" {
				t.Errorf("%s mentions @%s, which deploy/ozyd.yaml does not configure", r.UID, m)
			}
		}
	}
}
