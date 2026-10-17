package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// ProvisionStore is what provisioning needs from a database.
type ProvisionStore interface {
	UpsertProvisionedMonitor(ctx context.Context, row meta.MonitorRow, now time.Time) (changed bool, err error)
	ReleaseOrphanedMonitors(ctx context.Context, keep []string, now time.Time) ([]string, error)
}

// ProvisionResult reports a provisioning pass.
type ProvisionResult struct {
	Loaded, Changed, Failed int
	// Released names provisioned monitors whose file is gone.
	Released []string
}

// File is a provisioned monitor: a definition plus whether it runs.
type File struct {
	spec.Monitor
	Enabled *bool `json:"enabled,omitempty"`
}

// Provision reads every *.json directly in each directory and upserts it by
// uid, with dashboards' rules (internal/dashboard.Provision): one bad file is
// logged and skipped, a missing directory is skipped quietly, files load in
// sorted order so a duplicate uid resolves the same way every time.
//
// known lists the configured notifier names. A mention of anything else is a
// warning, not a refusal: the file may be shared by environments with
// different notifiers, and refusing would leave this one with no monitor at
// all — strictly worse than a monitor whose notification for that name fails
// with a notify_failed event saying why.
func Provision(ctx context.Context, store ProvisionStore, paths []string, known []string, now time.Time, log *slog.Logger) ProvisionResult {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var res ProvisionResult
	var seen []string
	// clean: every directory was read and every file loaded. Only then is a
	// monitor missing from the files known to be removed. A missing directory
	// may be a mount that did not happen this boot, and a broken file is a
	// monitor someone is fixing; releasing either would detach the monitor
	// from its file for good (the file, fixed, is then refused as a uid the
	// API owns).
	clean := true
	for _, dir := range paths {
		files, err := jsonFiles(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			log.Info("provisioning monitors: no such directory, skipping", "path", dir)
			clean = false
			continue
		case err != nil:
			log.Error("provisioning monitors: cannot read directory", "path", dir, "err", err)
			res.Failed++
			clean = false
			continue
		}
		for _, file := range files {
			if err := provisionFile(ctx, store, file, known, now, log, &res, &seen); err != nil {
				log.Error("provisioning: skipping a monitor", "file", file, "err", err)
				res.Failed++
			}
		}
	}
	if clean && res.Failed == 0 {
		released, err := store.ReleaseOrphanedMonitors(ctx, seen, now)
		if err != nil {
			log.Error("provisioning monitors: releasing monitors whose file is gone", "err", err)
		}
		for _, name := range released {
			log.Warn("provisioning: a provisioned monitor's file is gone; it keeps running and is now managed through the API",
				"monitor", name)
		}
		res.Released = released
	}
	if res.Loaded > 0 || res.Failed > 0 {
		log.Info("provisioned monitors", "loaded", res.Loaded, "changed", res.Changed, "failed", res.Failed)
	}
	return res
}

func provisionFile(ctx context.Context, store ProvisionStore, file string, known []string, now time.Time,
	log *slog.Logger, res *ProvisionResult, seen *[]string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("monitor definition: %w", err)
	}
	if f.UID == "" {
		return errors.New(`a provisioned monitor needs a "uid": it is what the upsert is keyed on`)
	}
	*seen = append(*seen, f.UID)
	if _, err := spec.Compile(f.Monitor); err != nil {
		return err
	}
	for _, m := range spec.Mentions(f.Message) {
		if !contains(known, m) {
			log.Warn("provisioning: monitor mentions a notifier this server does not have; those notifications will fail",
				"file", file, "notifier", m)
		}
	}
	opts, _ := json.Marshal(f.Options)
	changed, err := store.UpsertProvisionedMonitor(ctx, meta.MonitorRow{UID: f.UID, Name: f.Name, Type: string(f.Type),
		Query: f.Query, Options: opts, Message: f.Message, Tags: f.Tags, Enabled: f.Enabled == nil || *f.Enabled}, now)
	if err != nil {
		return err
	}
	res.Loaded++
	if changed {
		res.Changed++
	}
	return nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}
