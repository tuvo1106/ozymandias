package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/tuvo1106/ozymandias/internal/backup"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// ErrBackupRunning is returned when a backup is already in progress.
var ErrBackupRunning = errors.New("a backup is already running")

// backupName is what a backup may be called: one path element, no dots-only
// names, nothing a filesystem or a shell would read as structure.
var backupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// backuper is a store that can write a consistent copy of itself.
type backuper interface{ Backup(dst string) error }

// backupState is the server's backup bookkeeping: one at a time, and what the
// last one did, as self-metrics registered from the start.
type backupState struct {
	mu     sync.Mutex
	closed bool // set by Close, under mu: no backup starts on closed stores
	// lastOK is the unix time the last successful backup started; 0 is never.
	lastOK atomic.Int64
}

// Backup makes a consistent copy of everything ozyd stores under dir (which
// must be empty or absent) while ozyd keeps serving, and returns its manifest.
//
// The order is the point (internal/backup/doc.go): the queue is cut first and
// the stores after, so that a store's snapshot is never older than the queue's
// committed offsets. A restore then redelivers anything the stores have and the
// queue also still holds, and every store applies a record idempotently. The
// other order would lose data.
func (s *Server) Backup(ctx context.Context, dir string) (m backup.Manifest, err error) {
	if !s.bk.mu.TryLock() {
		return m, ErrBackupRunning
	}
	defer s.bk.mu.Unlock()
	if s.bk.closed {
		return m, errors.New("backup: the server is shutting down")
	}
	start := s.clock.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		s.reg.Counter("ozy.backup.runs", "result:"+result).Inc()
		if err == nil {
			s.bk.lastOK.Store(start.Unix())
		}
	}()
	if err = backup.PrepareDir(dir); err != nil {
		return m, err
	}
	// PrepareDir made sure dir was empty or absent, so everything in it from here
	// is this run's. A run that fails leaves no half-backup behind: a retry under
	// the same name would otherwise be told the name is taken, and the partial copy
	// (links and checkpoints included) would sit on the disk until someone found it.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	s.log.Info("backup: starting", "component", Component, "dir", dir)

	if s.pipe != nil {
		for _, name := range intake.Topics {
			if err = s.pipe.topics[name].Backup(filepath.Join(dir, queueDir, name)); err != nil {
				return m, fmt.Errorf("backup: queue %s: %w", name, err)
			}
		}
	}
	steps := []struct {
		name string
		do   func() error
	}{
		{"logs", func() error { return s.logs.Backup(filepath.Join(dir, logDir)) }},
		{"tsdb", func() error {
			b, ok := s.store.(backuper)
			if !ok {
				return errors.New("the configured metric store (metric_store: naive) cannot be backed up online")
			}
			return b.Backup(filepath.Join(dir, tsdbDir))
		}},
		{"sketches", func() error { return s.sketches.Backup(filepath.Join(dir, sketchDir)) }},
		{"traces", func() error { return s.traces.Backup(filepath.Join(dir, traceDir)) }},
		{"meta", func() error { return s.meta.Backup(ctx, filepath.Join(dir, metaFile)) }},
	}
	for _, st := range steps {
		if err = ctx.Err(); err != nil {
			return m, err
		}
		if err = st.do(); err != nil {
			return m, fmt.Errorf("backup: %s: %w", st.name, err)
		}
	}
	if m, err = backup.Finish(dir, buildinfo.Version, start); err != nil {
		return m, err
	}
	s.log.Info("backup: done", "component", Component, "dir", dir, "files", len(m.Files), "took", s.clock.Now().Sub(start))
	return m, nil
}

// registerBackup mounts POST /api/v1/admin/backup when backup.root is set, and
// registers the backup metrics. Backups are made under root, named by the
// request (or by the time), and never anywhere else.
func (s *Server) registerBackup(mux *http.ServeMux) {
	s.reg.Counter("ozy.backup.runs", "result:ok")
	s.reg.Counter("ozy.backup.runs", "result:error")
	// A timestamp, not an age: zero means "never", which an age of zero seconds
	// would pass for "just now".
	s.reg.GaugeFunc("ozy.backup.last_success_timestamp", func() float64 { return float64(s.bk.lastOK.Load()) })
	if s.cfg.Backup.Root != "" {
		mux.HandleFunc("POST /api/v1/admin/backup", s.handleBackup)
	}
}

// backupRequest is the body of POST /api/v1/admin/backup.
type backupRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeAdminError(w, http.StatusBadRequest, `expected {"name": ...} or an empty body`)
			return
		}
	}
	if req.Name == "" {
		req.Name = s.clock.Now().UTC().Format("20060102T150405Z")
	}
	if !backupName.MatchString(req.Name) {
		writeAdminError(w, http.StatusBadRequest, "name must be 1-64 characters of letters, digits, '.', '_' or '-', starting with a letter or digit")
		return
	}
	dir := filepath.Join(s.cfg.Backup.Root, req.Name)
	m, err := s.Backup(r.Context(), dir)
	switch {
	case errors.Is(err, ErrBackupRunning):
		w.Header().Set("Retry-After", "30")
		writeAdminError(w, http.StatusConflict, err.Error())
	case errors.Is(err, backup.ErrNotEmpty):
		writeAdminError(w, http.StatusConflict, "a backup named "+req.Name+" already exists")
	case err != nil:
		s.log.Error("backup failed", "component", Component, "err", err)
		writeAdminError(w, http.StatusInternalServerError, err.Error())
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "name": req.Name, "dir": dir, "files": len(m.Files), "summary": m.Summary(),
		})
	}
}

func writeAdminError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Status: "error", Error: msg})
}
