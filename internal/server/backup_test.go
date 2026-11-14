package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/backup"
	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

const (
	bkSeries = `{"series":[{"metric":"bk.count","type":"count","interval":10,"tags":["env:t"],"points":[[1790000000,3]]}]}`
	bkLogs   = `{"logs":[{"ts":1790000000000,"message":"backed up line","status":"info","service":"svc"}]}`
)

func backupConfig(t *testing.T) config.Ozyd {
	cfg := testConfig(t)
	cfg.Backup.Root = t.TempDir()
	return cfg
}

func startBackup(t *testing.T, h http.Handler, name string) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if name != "" {
		body = `{"name":"` + name + `"}`
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/admin/backup", strings.NewReader(body)))
	return rec
}

func restored(t *testing.T, cfg config.Ozyd, name string) *Server {
	t.Helper()
	cfg.DataDir = filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(filepath.Join(cfg.Backup.Root, name), cfg.DataDir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatalf("the restored data_dir did not start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Everything accepted before a backup is in the restored server, whichever mode
// it ran in: stored already (direct), or still in the queue (queued, with no
// consumer ever started), where the restored server's consumers apply it.
func TestBackup_RestoredServerHoldsEverythingAcknowledged(t *testing.T) {
	cfg := backupConfig(t)
	s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s.Handler(), "/v1/series", bkSeries); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := post(t, s.Handler(), "/v1/logs", bkLogs); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec := startBackup(t, s.Handler(), "b1")
	if rec.Code != http.StatusOK {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body)
	}
	_ = s.Close()

	m, err := backup.Verify(filepath.Join(cfg.Backup.Root, "b1"))
	if err != nil {
		t.Fatal(err)
	}
	prefixes := map[string]bool{}
	for _, f := range m.Files {
		prefixes[strings.SplitN(f.Path, "/", 2)[0]] = true
	}
	for _, want := range []string{"meta.db", "tsdb", "logs", "sketches", "traces"} {
		if !prefixes[want] {
			t.Errorf("the backup has nothing under %q: %v", want, prefixes)
		}
	}
	if cfg.Pipeline.Mode == config.PipelineQueued && !prefixes["queue"] {
		t.Error("a queued server's backup has no queue")
	}

	r := restored(t, cfg, "b1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, mustListen(t)) }()
	defer func() { cancel(); <-done }()
	testutil.Eventually(t, 10*time.Second, func() bool { return seriesCount(t, r.Handler(), "bk.count") == 1 },
		"the series acknowledged before the backup is not in the restored server")
	testutil.Eventually(t, 10*time.Second, func() bool {
		return bytes.Contains(get(t, r.Handler(), "/api/v1/logs?q=service:svc").Body.Bytes(), []byte("backed up line"))
	}, "the log acknowledged before the backup is not in the restored server")
}

// What a queued server had already applied is in the stores' snapshots, not only
// in the queue's: the restored server serves it with no consumer running.
func TestBackup_StoredDataSurvivesWithoutTheQueue(t *testing.T) {
	cfg := backupConfig(t)
	cfg.Pipeline.Mode = config.PipelineQueued
	cfg.Pipeline.Queue.SyncInterval = -1
	s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	feedStores(t, s)
	post(t, s.Handler(), "/v1/series", bkSeries)
	post(t, s.Handler(), "/v1/logs", bkLogs)
	testutil.Eventually(t, 10*time.Second, func() bool {
		return seriesCount(t, s.Handler(), "bk.count") == 1 &&
			bytes.Contains(get(t, s.Handler(), "/api/v1/logs?q=service:svc").Body.Bytes(), []byte("backed up line"))
	}, "never applied")
	if rec := startBackup(t, s.Handler(), ""); rec.Code != http.StatusOK {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Name string }
	_ = json.Unmarshal(startBackup(t, s.Handler(), "second").Body.Bytes(), &out)

	r := restored(t, cfg, "second") // New only: no consumer runs, so only the stores can answer
	if seriesCount(t, r.Handler(), "bk.count") != 1 {
		t.Fatal("the stored series is not in the restored server's store")
	}
	if !bytes.Contains(get(t, r.Handler(), "/api/v1/logs?q=service:svc").Body.Bytes(), []byte("backed up line")) {
		t.Fatal("the stored log is not in the restored server's store")
	}
}

func TestBackup_EndpointRefusesWhatItShould(t *testing.T) {
	cfg := backupConfig(t)
	s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, bad := range []string{"../escape", "a/b", ".hidden", "", " ", strings.Repeat("x", 65), "with space"} {
		if bad == "" {
			continue // an empty name means "name it by the time"
		}
		if rec := startBackup(t, s.Handler(), bad); rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: %d, want 400", bad, rec.Code)
		}
	}
	if rec := startBackup(t, s.Handler(), "dup"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := startBackup(t, s.Handler(), "dup"); rec.Code != http.StatusConflict {
		t.Errorf("a second backup under the same name: %d, want 409", rec.Code)
	}
	if rec := post(t, s.Handler(), "/api/v1/admin/backup", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("garbage body: %d", rec.Code)
	}
}

// With no backup.root the endpoint does not exist: an open endpoint that writes
// a tree of files is not something to leave mounted by default.
func TestBackup_OffMeansAbsent(t *testing.T) {
	s, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if rec := startBackup(t, s.Handler(), "x"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
}

func TestBackup_OneAtATimeAndCounted(t *testing.T) {
	cfg := backupConfig(t)
	s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.bk.mu.Lock()
	busy := make(chan error, 1)
	go func() {
		_, err := s.Backup(context.Background(), filepath.Join(cfg.Backup.Root, "x"))
		busy <- err
	}()
	select {
	case err := <-busy:
		if !errors.Is(err, ErrBackupRunning) {
			t.Errorf("a second concurrent backup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second backup waited for the first instead of being refused")
	}
	s.bk.mu.Unlock()
	if _, err := s.Backup(context.Background(), filepath.Join(cfg.Backup.Root, "y")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(context.Background(), filepath.Join(cfg.Backup.Root, "y")); err == nil {
		t.Fatal("into an existing backup")
	}
	got := map[string]float64{}
	for _, p := range s.reg.Snapshot() {
		if p.Name == "ozy.backup.runs" {
			for _, tg := range p.Tags {
				got[tg] = p.Value
			}
		}
		if p.Name == "ozy.backup.last_success_timestamp" && p.Value == 0 {
			t.Error("last_success_timestamp is still 0 after a success")
		}
	}
	if got["result:ok"] != 1 || got["result:error"] != 1 {
		t.Errorf("runs = %v, want 1 ok and 1 error (the one over an existing backup; the busy one never ran)", got)
	}
}

// A backup that fails leaves nothing behind, so the same name works on retry.
func TestBackup_AFailedRunLeavesNoPartialDirectory(t *testing.T) {
	cfg := backupConfig(t)
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	dir := filepath.Join(cfg.Backup.Root, "retry")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // fails after PrepareDir and the queue step, before the stores
	if _, err := s.Backup(ctx, dir); err == nil {
		t.Fatal("a cancelled backup succeeded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the failed backup left %s behind (%v)", dir, err)
	}
	if _, err := s.Backup(context.Background(), dir); err != nil {
		t.Fatalf("the retry under the same name: %v", err)
	}
}

// Close waits for a running backup, and a backup after Close is refused: the
// stores a backup reads must not be closed under it.
func TestBackup_CloseWaitsForARunningBackupAndRefusesNewOnes(t *testing.T) {
	cfg := backupConfig(t)
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	s.bk.mu.Lock() // a backup in progress
	closed := make(chan struct{})
	go func() { _ = s.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a backup held the stores")
	case <-time.After(150 * time.Millisecond):
	}
	s.bk.mu.Unlock()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned once the backup finished")
	}
	if _, err := s.Backup(context.Background(), filepath.Join(cfg.Backup.Root, "late")); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("a backup after Close: %v, want it refused as shutting down", err)
	}
}
