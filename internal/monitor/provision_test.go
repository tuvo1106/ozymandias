package monitor

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

type provisionDB struct{ *meta.DB }

func (p provisionDB) ReleaseOrphanedMonitors(ctx context.Context, keep []string, now time.Time) ([]string, error) {
	return p.DB.ReleaseOrphanedMonitors(ctx, keep, now)
}

func (p provisionDB) UpsertProvisionedMonitor(ctx context.Context, r meta.MonitorRow, now time.Time) (bool, error) {
	_, changed, err := p.DB.UpsertProvisionedMonitor(ctx, r, now)
	return changed, err
}

func TestProvision(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	good := `{"uid":"a","name":"A","type":"metric","query":"avg(last_5m):avg:x{*} > 1","message":"@hook @elsewhere"}`
	dir := testutil.TempDirWith(t, map[string]string{
		"a.json":        good,
		"disabled.json": `{"uid":"d","name":"D","type":"metric","query":"avg(last_5m):avg:x{*} > 1","enabled":false}`,
		"nouid.json":    `{"name":"N","type":"metric","query":"avg(last_5m):avg:x{*} > 1"}`,
		"bad.json":      `{"uid":"b","name":"B","type":"metric","query":"nonsense"}`,
		"typo.json":     `{"uid":"t","name":"T","type":"metric","query":"avg(last_5m):avg:x{*} > 1","optoins":{}}`,
		"notjson.json":  `{`,
		"README.md":     "ignored",
		"sub/x.json":    good,
	})
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	paths := []string{dir, filepath.Join(dir, "missing")}
	res := Provision(context.Background(), provisionDB{db}, paths, []string{"hook"}, t0, log)
	if res.Loaded != 2 || res.Changed != 2 || res.Failed != 4 {
		t.Fatalf("%+v\n%s", res, logs.String())
	}
	if !strings.Contains(logs.String(), "notifier=elsewhere") {
		t.Errorf("unknown mention not warned:\n%s", logs.String())
	}
	rows, _ := db.Monitors(context.Background())
	if len(rows) != 2 || !rows[0].Provisioned || rows[1].Enabled {
		t.Fatalf("rows %+v", rows)
	}
	// A restart with the same files changes nothing.
	if res := Provision(context.Background(), provisionDB{db}, paths, nil, t0, nil); res.Changed != 0 || res.Loaded != 2 {
		t.Fatalf("restart: %+v", res)
	}
	// An unreadable directory is a failure, not a skip.
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := Provision(context.Background(), provisionDB{db}, []string{filepath.Join(dir, "file")}, nil, t0, nil); res.Failed != 1 {
		t.Fatalf("not a directory: %+v", res)
	}
}
