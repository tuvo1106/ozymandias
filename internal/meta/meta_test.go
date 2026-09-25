package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var (
	ctx = context.Background()
	now = time.Unix(1790000000, 0)
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meta.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, path
}

func TestObserve_FirstSightingFixesTheType(t *testing.T) {
	d, _ := open(t)
	if err := d.Observe(ctx, "http.count", wire.KindCount, 10, now); err != nil {
		t.Fatal(err)
	}
	if err := d.Observe(ctx, "http.count", wire.KindCount, 20, now.Add(time.Hour)); err != nil {
		t.Fatalf("same type, new interval: %v", err)
	}
	err := d.Observe(ctx, "http.count", wire.KindGauge, 0, now)
	if !errors.Is(err, ErrTypeConflict) || !strings.Contains(err.Error(), "http.count is a count, not a gauge") {
		t.Fatalf("conflict: %v", err)
	}
	m, ok := d.Metric("http.count")
	if !ok || m.Type != wire.KindCount || m.Interval != 10 || !m.FirstSeen.Equal(now) {
		t.Fatalf("Metric = %+v, %v", m, ok)
	}
	if _, ok := d.Metric("nope"); ok {
		t.Fatal("unknown metric found")
	}
}

func TestOpen_ReloadsWhatWasRecorded(t *testing.T) {
	d, path := open(t)
	if err := d.Observe(ctx, "g", wire.KindGauge, 0, now); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if m, ok := d.Metric("g"); !ok || m.Type != wire.KindGauge {
		t.Fatalf("after reopen: %+v, %v", m, ok)
	}
	if err := d.Observe(ctx, "g", wire.KindCount, 10, now); !errors.Is(err, ErrTypeConflict) {
		t.Fatalf("conflict after reopen: %v", err)
	}
}

func TestObserve_ConcurrentFirstSightingsAgree(t *testing.T) {
	d, _ := open(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			kind := wire.KindCount
			if i%2 == 1 {
				kind = wire.KindGauge
			}
			errs <- d.Observe(ctx, "race", kind, 10, now)
		})
	}
	wg.Wait()
	close(errs)
	var ok, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrTypeConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if ok != 8 || conflicts != 8 {
		t.Fatalf("ok=%d conflicts=%d: exactly one type must win", ok, conflicts)
	}
}

func TestOpen_Errors(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no", "dir.db")); err == nil {
		t.Error("opened in a missing directory")
	}
	junk := filepath.Join(t.TempDir(), "junk.db")
	_ = os.WriteFile(junk, []byte(strings.Repeat("junk", 500)), 0o600)
	if _, err := Open(junk); err == nil {
		t.Error("opened a non-database")
	}
	d, _ := open(t)
	if _, err := d.db.Exec(`INSERT INTO metric_meta VALUES('x', 'gauge', 'not-a-number', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := d.load(); err == nil {
		t.Error("loaded a corrupt row")
	}
}

func TestObserve_WriteFailureIsReportedAndNotCached(t *testing.T) {
	d, _ := open(t)
	_ = d.db.Close()
	if err := d.Observe(ctx, "m", wire.KindGauge, 0, now); err == nil {
		t.Fatal("no error writing to a closed database")
	}
	if _, ok := d.Metric("m"); ok {
		t.Fatal("cached a metric that was never stored")
	}
}
