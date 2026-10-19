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
	"syscall"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// ProvisionStore is what provisioning needs from a database.
type ProvisionStore interface {
	UpsertProvisionedMonitor(ctx context.Context, row meta.MonitorRow, now time.Time) (changed bool, err error)
	ProvisionedMonitors(ctx context.Context) (map[string]string, error)
	ReleaseMonitors(ctx context.Context, uids []string, now time.Time) error
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
	// unknown says why some file's uid could not be read this pass: a
	// directory that is missing (a mount that did not happen this boot) or
	// unreadable, or a file that does not decode. Then a monitor absent from
	// seen may still have its file, and releasing it would detach it for good
	// (the file, fixed, is refused as a uid the API owns). A file that
	// decoded but failed later still had its uid seen, so it blocks nothing.
	var unknown []string
	for _, dir := range paths {
		files, err := jsonFiles(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			log.Info("provisioning monitors: no such directory, skipping", "path", dir)
			unknown = append(unknown, "directory "+dir+" does not exist")
			continue
		case err != nil:
			log.Error("provisioning monitors: cannot read directory", "path", dir, "err", err)
			res.Failed++
			unknown = append(unknown, "directory "+dir+" cannot be read")
			continue
		}
		for _, file := range files {
			err := provisionFile(ctx, store, file, known, now, log, &res, &seen)
			unknown = append(unknown, outcome(file, err, &res, log)...)
		}
	}
	res.Released = releaseOrphans(ctx, store, seen, unknown, now, log)
	if res.Loaded > 0 || res.Failed > 0 {
		log.Info("provisioned monitors", "loaded", res.Loaded, "changed", res.Changed, "failed", res.Failed)
	}
	return res
}

func provisionFile(ctx context.Context, store ProvisionStore, file string, known []string, now time.Time,
	log *slog.Logger, res *ProvisionResult, seen *[]string) error {
	// Lstat once, before reading, so a failed read is classified by what
	// the path was when it was read.
	fi, err := os.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return errVanished
	}
	if err != nil {
		return fmt.Errorf("%w (%w)", err, errNoUID)
	}
	link := fi.Mode()&fs.ModeSymlink != 0
	data, err := os.ReadFile(file)
	switch {
	case err != nil && link && errors.Is(err, syscall.EISDIR):
		// A symlink to a directory: not a monitor file, now or later.
		return err
	case err != nil && link && errors.Is(err, fs.ErrNotExist):
		// A dangling symlink: its target may be mid-replacement (a checkout
		// of the directory it points into), so its uid is unknown. If it is
		// dangling for good, the warning names it every pass.
		return errDangling
	case errors.Is(err, fs.ErrNotExist):
		// Removed between the Lstat and the read.
		return errVanished
	case err != nil:
		// Unreadable for now (permissions mid-edit, a flaky mount).
		return fmt.Errorf("%w (%w)", err, errNoUID)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		// An unknown field still leaves the uid readable leniently, and a
		// file whose uid is known is not one that was removed.
		var probe struct{ UID string }
		if json.Unmarshal(data, &probe) == nil && probe.UID != "" {
			*seen = append(*seen, probe.UID)
			return fmt.Errorf("monitor definition: %w", err)
		}
		return fmt.Errorf("monitor definition: %w (%w)", err, errNoUID)
	}
	if f.UID == "" {
		return fmt.Errorf(`a provisioned monitor needs a "uid": it is what the upsert is keyed on (%w)`, errNoUID)
	}
	*seen = append(*seen, f.UID)
	c, err := spec.Compile(f.Monitor)
	if err != nil {
		return err
	}
	for _, m := range c.Template.Recipients() {
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

// outcome records how one file went, and returns why its uid is unknown, if
// it is.
func outcome(file string, err error, res *ProvisionResult, log *slog.Logger) []string {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errVanished):
		log.Info("provisioning: a monitor file disappeared during the pass", "file", file)
		return []string{file + " disappeared during the pass"}
	case errors.Is(err, errDangling):
		log.Warn("provisioning: a monitor file is a symlink to nothing; until it is removed or its target returns, "+
			"monitors whose file is gone are not released", "file", file)
		return []string{file + " is a dangling symlink"}
	}
	log.Error("provisioning: skipping a monitor", "file", file, "err", err)
	res.Failed++
	if errors.Is(err, errNoUID) {
		return []string{file + " has no readable uid"}
	}
	return nil
}

// errNoUID marks a file whose uid could not be read.
var errNoUID = errors.New("uid unknown")

// errVanished is a file listed and then gone before it was read: removed, or
// being replaced (a checkout, an editor's rename-and-write). Its uid is
// unknown until the next pass, which is not an error worth logging as one.
var errVanished = fmt.Errorf("removed while provisioning ran; decided next pass (%w)", errNoUID)

// errDangling is a *.json symlink whose target does not exist.
var errDangling = fmt.Errorf("dangling symlink (%w)", errNoUID)

// releaseOrphans releases provisioned monitors that have no file, or, when
// some file's uid was unreadable this pass, says why it is not doing so.
func releaseOrphans(ctx context.Context, store ProvisionStore, seen, unknown []string, now time.Time, log *slog.Logger) []string {
	provisioned, err := store.ProvisionedMonitors(ctx)
	if err != nil {
		log.Error("provisioning monitors: listing provisioned monitors", "err", err)
		return nil
	}
	present := map[string]bool{}
	for _, uid := range seen {
		present[uid] = true
	}
	var uids, names []string
	for uid, name := range provisioned {
		if !present[uid] {
			uids = append(uids, uid)
			names = append(names, name)
		}
	}
	sort.Strings(uids)
	sort.Strings(names)
	if len(uids) == 0 {
		return nil
	}
	if len(unknown) > 0 {
		log.Warn("provisioning: provisioned monitors have no file, but are not released this time because a uid could not be read",
			"monitors", names, "because", unknown)
		return nil
	}
	if err := store.ReleaseMonitors(ctx, uids, now); err != nil {
		log.Error("provisioning monitors: releasing monitors whose file is gone", "err", err)
		return nil
	}
	for _, name := range names {
		log.Warn("provisioning: a provisioned monitor's file is gone; it keeps running and is now managed through the API",
			"monitor", name)
	}
	return names
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
